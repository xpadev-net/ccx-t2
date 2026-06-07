package web

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os/exec"
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
	if s.allowedOrigins[r.Header.Get("Origin")] {
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
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
	s.mux.HandleFunc("/api/workers", s.handleWorkers)
	s.mux.HandleFunc("/api/harnesses", s.handleHarnesses)
	s.mux.HandleFunc("/api/config", s.handleConfig)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, "ledger is not configured")
		return
	}
	tasks, err := s.ledger.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load tasks")
		return
	}
	out := make([]taskResponse, len(tasks))
	for i, task := range tasks {
		out[i] = taskResponseFromLedger(task)
	}
	writeJSON(w, http.StatusOK, out)
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
	Name      string         `json:"name"`
	Available bool           `json:"available"`
	Usage     map[string]any `json:"usage"`
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
		usage := map[string]any{"note": "unavailable"}
		if available {
			usage = map[string]any{"command": command}
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
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	return false
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
