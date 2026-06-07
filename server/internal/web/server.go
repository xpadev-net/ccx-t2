package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
	"github.com/xpadev/ccx-t2/internal/worker"
)

// Server serves the browser-facing REST API.
type Server struct {
	cfg            *config.Config
	ledger         *ledger.Ledger
	registry       *worker.Registry
	trigger        Triggerer
	secret         string
	authDisabled   bool
	allowedOrigins map[string]bool
	harnesses      []harnessResponse
	mux            *http.ServeMux
}

// Deps contains dependencies needed by the web API.
type Deps struct {
	Config         *config.Config
	Ledger         *ledger.Ledger
	Registry       *worker.Registry
	Trigger        Triggerer
	Secret         string
	AuthDisabled   bool
	AllowedOrigins []string
}

// New constructs a web API handler.
func New(deps Deps) *Server {
	s := &Server{
		cfg:            deps.Config,
		ledger:         deps.Ledger,
		registry:       deps.Registry,
		trigger:        deps.Trigger,
		secret:         deps.Secret,
		authDisabled:   deps.AuthDisabled,
		allowedOrigins: allowedOriginSet(deps.AllowedOrigins),
		harnesses:      harnessResponsesFromConfig(deps.Config),
		mux:            http.NewServeMux(),
	}
	if s.secret == "" && !s.authDisabled {
		log.Printf("web: bearer auth is not configured; set Secret or AuthDisabled=true explicitly")
	}
	s.routes()
	return s
}

// Triggerer starts or wakes the orchestrator after a web mutation.
type Triggerer interface {
	Trigger(ctx context.Context, reason string) error
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.authorize(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.authDisabled {
		return true
	}
	if s.secret == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	hdr := r.Header.Get("Authorization")
	if !strings.HasPrefix(hdr, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	token := hdr[len("Bearer "):]
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.secret)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (s *Server) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "Origin")
	if s.allowedOrigins[r.Header.Get("Origin")] {
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
}

func allowedOriginSet(origins []string) map[string]bool {
	out := make(map[string]bool, len(origins))
	for _, origin := range origins {
		if origin != "" {
			out[origin] = true
		}
	}
	return out
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/tasks", s.handleTasks)
	s.mux.HandleFunc("/api/tasks/", s.handleTaskByID)
	s.mux.HandleFunc("/api/workers", s.handleWorkers)
	s.mux.HandleFunc("/api/harnesses", s.handleHarnesses)
	s.mux.HandleFunc("/api/config", s.handleConfig)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getTasks(w, r)
	case http.MethodPost:
		s.createTask(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		s.updateTask(w, r, id)
	default:
		methodNotAllowed(w, http.MethodPatch)
	}
}

func (s *Server) getTasks(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, "ledger is not configured")
		return
	}
	tasks, err := s.ledger.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load tasks")
		return
	}
	writeJSON(w, http.StatusOK, taskResponsesFromLedger(tasks))
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, "ledger is not configured")
		return
	}
	var req taskMutationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	task, err := taskFromCreateRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if task.ID != "" {
		existing, ok, err := s.loadTaskIfExists(task.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load tasks")
			return
		}
		if ok {
			w.Header().Set("Idempotency-Key", task.ID)
			s.writeTaskCreateResponse(w, r, http.StatusOK, existing)
			return
		}
	}
	id := task.ID
	if id != "" {
		archived, err := s.ledger.IsArchived(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "check archived task")
			return
		}
		if archived {
			writeError(w, http.StatusConflict, "task already exists in archive")
			return
		}
		if err := s.ledger.Add(task); err != nil {
			if errors.Is(err, ledger.ErrTaskExists) {
				existing, ok, loadErr := s.loadTaskIfExists(id)
				if loadErr != nil {
					writeError(w, http.StatusInternalServerError, "load tasks")
					return
				}
				if ok {
					w.Header().Set("Idempotency-Key", id)
					s.writeTaskCreateResponse(w, r, http.StatusOK, existing)
					return
				}
				writeError(w, http.StatusConflict, "task already exists outside active ledger")
				return
			}
			writeError(w, http.StatusInternalServerError, "create task")
			return
		}
	} else {
		var err error
		id, err = s.ledger.AddNew(task)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "create task")
			return
		}
	}

	task.ID = id
	task.UpdatedAt = ""
	w.Header().Set("Idempotency-Key", id)
	s.writeTaskCreateResponse(w, r, http.StatusCreated, task)
}

func (s *Server) writeTaskCreateResponse(w http.ResponseWriter, r *http.Request, status int, task ledger.Task) {
	resp := taskCreateResponse{Task: taskResponseFromLedger(task)}
	if s.trigger != nil {
		if err := s.trigger.Trigger(context.WithoutCancel(r.Context()), "task created: "+task.ID); err != nil {
			log.Printf("web: orchestrator trigger after task create failed: %v", err)
			resp.TriggerError = "orchestrator trigger failed"
			status = http.StatusAccepted
		} else {
			resp.OrchestratorTriggered = true
		}
	}
	writeJSON(w, status, resp)
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request, id string) {
	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, "ledger is not configured")
		return
	}
	var req taskMutationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.IdempotencyKey != nil {
		writeError(w, http.StatusBadRequest, "idempotency_key is only supported for task creation")
		return
	}
	fields := fieldsFromUpdateRequest(req)
	if len(fields) == 0 {
		writeError(w, http.StatusBadRequest, "no task fields provided")
		return
	}
	if status, ok := fields["status"].(string); ok && strings.TrimSpace(status) == "" {
		writeError(w, http.StatusBadRequest, "status cannot be empty")
		return
	}
	prev, err := s.ledger.UpdateReturnPrevWith(id, func(current ledger.Task) (map[string]any, error) {
		if err := validateTaskUpdate(current, fields); err != nil {
			return nil, err
		}
		return fields, nil
	})
	if err != nil {
		if errors.Is(err, ledger.ErrTaskNotFound) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		if errors.Is(err, errInvalidTaskMutation) {
			writeError(w, http.StatusBadRequest, invalidTaskMutationMessage(err))
			return
		}
		writeError(w, http.StatusInternalServerError, "update task")
		return
	}
	updated := taskSnapshotWithFields(prev, fields)
	updated.UpdatedAt = ""
	writeJSON(w, http.StatusOK, taskResponseFromLedger(updated))
}

func (s *Server) loadTaskIfExists(id string) (ledger.Task, bool, error) {
	return s.ledger.LoadByID(id)
}

func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.registry == nil {
		writeError(w, http.StatusInternalServerError, "worker registry is not configured")
		return
	}
	workers := s.registry.All()
	sort.Slice(workers, func(i, j int) bool {
		return workers[i].WorkerID < workers[j].WorkerID
	})
	out := make([]workerResponse, len(workers))
	for i, info := range workers {
		out[i] = workerResponseFromRegistry(info)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHarnesses(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.cfg == nil {
		writeError(w, http.StatusInternalServerError, "config is not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.harnesses)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.cfg == nil {
		writeError(w, http.StatusInternalServerError, "config is not configured")
		return
	}
	writeJSON(w, http.StatusOK, configResponseFromConfig(s.cfg))
}

type taskResponse struct {
	ID             string   `json:"id"`
	Title          string   `json:"title,omitempty"`
	Status         string   `json:"status,omitempty"`
	Branch         string   `json:"branch,omitempty"`
	WorkerID       string   `json:"worker_id,omitempty"`
	Harness        string   `json:"harness,omitempty"`
	AllowedFiles   []string `json:"allowed_files,omitempty"`
	ForbiddenFiles []string `json:"forbidden_files,omitempty"`
	PrURL          string   `json:"pr_url,omitempty"`
	MergeCommit    string   `json:"merge_commit,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	UpdatedAt      string   `json:"updated_at,omitempty"`
	Body           string   `json:"body,omitempty"`
}

type taskCreateResponse struct {
	Task                  taskResponse `json:"task"`
	OrchestratorTriggered bool         `json:"orchestrator_triggered"`
	TriggerError          string       `json:"trigger_error,omitempty"`
}

type taskMutationRequest struct {
	IdempotencyKey *string   `json:"idempotency_key"`
	Title          *string   `json:"title"`
	Status         *string   `json:"status"`
	Branch         *string   `json:"branch"`
	WorkerID       *string   `json:"worker_id"`
	Harness        *string   `json:"harness"`
	AllowedFiles   *[]string `json:"allowed_files"`
	ForbiddenFiles *[]string `json:"forbidden_files"`
	PrURL          *string   `json:"pr_url"`
	MergeCommit    *string   `json:"merge_commit"`
	Reason         *string   `json:"reason"`
	Body           *string   `json:"body"`
}

func taskFromCreateRequest(req taskMutationRequest) (ledger.Task, error) {
	if req.IdempotencyKey != nil && strings.TrimSpace(*req.IdempotencyKey) == "" {
		return ledger.Task{}, fmt.Errorf("idempotency_key cannot be empty")
	}
	task := ledger.Task{Status: "unstarted"}
	if req.IdempotencyKey != nil {
		task.ID = strings.TrimSpace(*req.IdempotencyKey)
		if !reWebTaskID.MatchString(task.ID) {
			return ledger.Task{}, fmt.Errorf("idempotency_key must be a task ID like task-YYYYMMDD-0001")
		}
	}
	fields := fieldsFromUpdateRequest(req)
	task = taskSnapshotWithFields(task, fields)
	task.UpdatedAt = ""
	if strings.TrimSpace(task.Status) == "" {
		task.Status = "unstarted"
	}
	if strings.TrimSpace(task.Title) == "" && strings.TrimSpace(task.Body) == "" {
		return ledger.Task{}, fmt.Errorf("title or body is required")
	}
	return task, nil
}

func taskSnapshotWithFields(task ledger.Task, fields map[string]any) ledger.Task {
	if title, ok := fields["title"].(string); ok {
		task.Title = title
	}
	if status, ok := fields["status"].(string); ok {
		task.Status = status
	}
	if branch, ok := fields["branch"].(string); ok {
		task.Branch = branch
	}
	if workerID, ok := fields["worker_id"].(string); ok {
		task.WorkerID = workerID
	}
	if harness, ok := fields["harness"].(string); ok {
		task.Harness = harness
	}
	if allowedFiles, ok := fields["allowed_files"].([]string); ok {
		task.AllowedFiles = allowedFiles
	}
	if forbiddenFiles, ok := fields["forbidden_files"].([]string); ok {
		task.ForbiddenFiles = forbiddenFiles
	}
	if prURL, ok := fields["pr_url"].(string); ok {
		task.PrURL = prURL
	}
	if mergeCommit, ok := fields["merge_commit"].(string); ok {
		task.MergeCommit = mergeCommit
	}
	if reason, ok := fields["reason"].(string); ok {
		task.Reason = reason
	}
	if body, ok := fields["body"].(string); ok {
		task.Body = body
	}
	return task
}

var reWebTaskID = regexp.MustCompile(`^task-\d{8}-\d{4}$`)

var errInvalidTaskMutation = errors.New("invalid task mutation")

func validateTaskUpdate(current ledger.Task, fields map[string]any) error {
	if status, ok := fields["status"].(string); ok && strings.TrimSpace(status) == "" {
		return fmt.Errorf("%w: status cannot be empty", errInvalidTaskMutation)
	}
	next := taskSnapshotWithFields(current, fields)
	if strings.TrimSpace(next.Title) == "" && strings.TrimSpace(next.Body) == "" {
		return fmt.Errorf("%w: title and body cannot both be empty", errInvalidTaskMutation)
	}
	return nil
}

func invalidTaskMutationMessage(err error) string {
	msg := err.Error()
	prefix := errInvalidTaskMutation.Error() + ": "
	return strings.TrimPrefix(msg, prefix)
}

func fieldsFromUpdateRequest(req taskMutationRequest) map[string]any {
	fields := make(map[string]any)
	if req.Title != nil {
		fields["title"] = *req.Title
	}
	if req.Status != nil {
		fields["status"] = *req.Status
	}
	if req.Branch != nil {
		fields["branch"] = *req.Branch
	}
	if req.WorkerID != nil {
		fields["worker_id"] = *req.WorkerID
	}
	if req.Harness != nil {
		fields["harness"] = *req.Harness
	}
	if req.AllowedFiles != nil {
		fields["allowed_files"] = *req.AllowedFiles
	}
	if req.ForbiddenFiles != nil {
		fields["forbidden_files"] = *req.ForbiddenFiles
	}
	if req.PrURL != nil {
		fields["pr_url"] = *req.PrURL
	}
	if req.MergeCommit != nil {
		fields["merge_commit"] = *req.MergeCommit
	}
	if req.Reason != nil {
		fields["reason"] = *req.Reason
	}
	if req.Body != nil {
		fields["body"] = strings.Trim(*req.Body, "\n")
	}
	return fields
}

func taskResponsesFromLedger(tasks []ledger.Task) []taskResponse {
	out := make([]taskResponse, len(tasks))
	for i, task := range tasks {
		out[i] = taskResponseFromLedger(task)
	}
	return out
}

type workerResponse struct {
	TaskID    string `json:"task_id"`
	WorkerID  string `json:"worker_id"`
	Harness   string `json:"harness"`
	StartedAt string `json:"started_at"`
}

func workerResponseFromRegistry(info worker.Info) workerResponse {
	return workerResponse{
		TaskID:    info.TaskID,
		WorkerID:  info.WorkerID,
		Harness:   info.Harness,
		StartedAt: info.StartedAt.Format(time.RFC3339Nano),
	}
}

func taskResponseFromLedger(task ledger.Task) taskResponse {
	return taskResponse{
		ID:             task.ID,
		Title:          task.Title,
		Status:         task.Status,
		Branch:         task.Branch,
		WorkerID:       task.WorkerID,
		Harness:        task.Harness,
		AllowedFiles:   task.AllowedFiles,
		ForbiddenFiles: task.ForbiddenFiles,
		PrURL:          task.PrURL,
		MergeCommit:    task.MergeCommit,
		Reason:         task.Reason,
		UpdatedAt:      task.UpdatedAt,
		Body:           task.Body,
	}
}

type configResponse struct {
	Project         projectConfigResponse            `json:"project"`
	Server          serverConfigResponse             `json:"server"`
	Orchestrator    orchestratorConfigResponse       `json:"orchestrator"`
	WorkerHarnesses []string                         `json:"worker_harnesses"`
	Harnesses       map[string]harnessConfigResponse `json:"harnesses"`
	GitHub          githubConfigResponse             `json:"github"`
}

type projectConfigResponse struct {
	Slug         string `json:"slug"`
	RepoPath     string `json:"repo_path"`
	WorktreeBase string `json:"worktree_base"`
}

type serverConfigResponse struct {
	Port int `json:"port"`
}

type orchestratorConfigResponse struct {
	Harness           string `json:"harness"`
	HeartbeatInterval string `json:"heartbeat_interval"`
	Timeout           string `json:"timeout"`
}

type githubConfigResponse struct {
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
}

type harnessConfigResponse struct {
	Command string `json:"command"`
}

type harnessResponse struct {
	Name      string       `json:"name"`
	Available bool         `json:"available"`
	Usage     harnessUsage `json:"usage"`
}

type harnessUsage struct {
	Command string `json:"command,omitempty"`
	Note    string `json:"note,omitempty"`
}

func harnessResponsesFromConfig(cfg *config.Config) []harnessResponse {
	if cfg == nil {
		return nil
	}
	out := make([]harnessResponse, 0, len(cfg.WorkerHarnesses))
	for _, name := range cfg.WorkerHarnesses {
		h, ok := cfg.Harnesses[name]
		available := false
		command := ""
		if ok {
			command = h.Command
			if command != "" {
				_, err := exec.LookPath(command)
				available = err == nil
			}
		}
		usage := harnessUsage{Note: "unavailable"}
		if available {
			usage = harnessUsage{Command: command}
		}
		out = append(out, harnessResponse{
			Name:      name,
			Available: available,
			Usage:     usage,
		})
	}
	return out
}

func configResponseFromConfig(cfg *config.Config) configResponse {
	workerHarnesses := append([]string(nil), cfg.WorkerHarnesses...)
	harnesses := make(map[string]harnessConfigResponse, len(cfg.Harnesses))
	for name, h := range cfg.Harnesses {
		harnesses[name] = harnessConfigResponse{Command: h.Command}
	}
	return configResponse{
		Project: projectConfigResponse{
			Slug:         cfg.Project.Slug,
			RepoPath:     cfg.Project.RepoPath,
			WorktreeBase: cfg.Project.WorktreeBase,
		},
		Server: serverConfigResponse{
			Port: cfg.Server.Port,
		},
		Orchestrator: orchestratorConfigResponse{
			Harness:           cfg.Orchestrator.Harness,
			HeartbeatInterval: cfg.Orchestrator.HeartbeatInterval.String(),
			Timeout:           cfg.Orchestrator.Timeout.String(),
		},
		WorkerHarnesses: workerHarnesses,
		Harnesses:       harnesses,
		GitHub: githubConfigResponse{
			Owner: cfg.GitHub.Owner,
			Repo:  cfg.GitHub.Repo,
		},
	}
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	methodNotAllowed(w, method)
	return false
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	limited := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return fmt.Errorf("%w: limit %d bytes", errRequestBodyTooLarge, maxBytesErr.Limit)
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid JSON: multiple values")
	}
	return nil
}

var errRequestBodyTooLarge = errors.New("request body too large")

func writeDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errRequestBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("web: json encode error: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
