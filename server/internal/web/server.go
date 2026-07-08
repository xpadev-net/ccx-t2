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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
	runtimepkg "github.com/xpadev/ccx-t2/internal/runtime"
	"github.com/xpadev/ccx-t2/internal/tmux"
	"github.com/xpadev/ccx-t2/internal/worker"
	"github.com/xpadev/ccx-t2/internal/worktree"
)

// Server serves the browser-facing REST API.
type Server struct {
	configMu        sync.RWMutex
	cfg             *config.Config
	configPath      string
	ledger          *ledger.Ledger
	manager         *runtimepkg.Manager
	registry        *worker.Registry
	trigger         Triggerer
	cleaner         WorkerCleaner
	pipeOutput      PipeOutputFunc
	pipeBytes       PipeBytesFunc
	capturePane     CapturePaneFunc
	sendKeys        SendKeysFunc
	sendRawKeys     SendRawKeysFunc
	resizePane      ResizePaneFunc
	isSessionAlive  SessionAliveFunc
	isWindowAlive   WindowAliveFunc
	isPaneIdle      PaneIdleFunc
	tmuxSession     string
	projectScoped   bool
	pendingTriggers *pendingTriggerSet
	secret          string
	authDisabled    bool
	allowedOrigins  map[string]bool
	harnesses       []harnessResponse
	mux             *http.ServeMux
	ledgerClientsMu sync.Mutex
	ledgerClients   map[string]map[*ledgerWSClient]struct{}
	tmuxStreams     *tmuxStreamRegistry
	startLocks      *orchestratorStartLocks
}

const deleteCleanupLease = 5 * time.Minute
const deleteCleanupTimeout = 4 * time.Minute
const maxFollowupMessageBytes = 20000
const followupTmuxOperationTimeout = 5 * time.Second

// Deps contains dependencies needed by the web API.
type Deps struct {
	Config         *config.Config
	ConfigPath     string
	Ledger         *ledger.Ledger
	Manager        *runtimepkg.Manager
	Registry       *worker.Registry
	Trigger        Triggerer
	Cleaner        WorkerCleaner
	PipeOutput     PipeOutputFunc
	PipeBytes      PipeBytesFunc
	CapturePane    CapturePaneFunc
	SendKeys       SendKeysFunc
	SendRawKeys    SendRawKeysFunc
	ResizePane     ResizePaneFunc
	IsSessionAlive SessionAliveFunc
	IsWindowAlive  WindowAliveFunc
	IsPaneIdle     PaneIdleFunc
	Session        string
	Secret         string
	AuthDisabled   bool
	AllowedOrigins []string
}

// New constructs a web API handler.
func New(deps Deps) *Server {
	cfg := config.Clone(deps.Config)
	s := &Server{
		cfg:            cfg,
		configPath:     deps.ConfigPath,
		ledger:         deps.Ledger,
		manager:        deps.Manager,
		registry:       deps.Registry,
		trigger:        deps.Trigger,
		pipeOutput:     deps.PipeOutput,
		pipeBytes:      deps.PipeBytes,
		capturePane:    deps.CapturePane,
		sendKeys:       deps.SendKeys,
		sendRawKeys:    deps.SendRawKeys,
		resizePane:     deps.ResizePane,
		isSessionAlive: deps.IsSessionAlive,
		isWindowAlive:  deps.IsWindowAlive,
		isPaneIdle:     deps.IsPaneIdle,
		tmuxSession:    deps.Session,
		secret:         deps.Secret,
		authDisabled:   deps.AuthDisabled,
		allowedOrigins: allowedOriginSet(deps.AllowedOrigins),
		harnesses:      harnessResponsesFromConfig(cfg),
		mux:            http.NewServeMux(),
		tmuxStreams:    &tmuxStreamRegistry{},
		startLocks:     &orchestratorStartLocks{},
		pendingTriggers: &pendingTriggerSet{
			tasks: make(map[string]triggerState),
		},
	}
	s.cleaner = workerCleanerFromDeps(s, deps)
	if s.pipeOutput == nil {
		s.pipeOutput = tmux.PipeOutput
	}
	if s.pipeBytes == nil {
		if deps.PipeOutput != nil {
			s.pipeBytes = pipeLinesAsBytes(deps.PipeOutput)
		} else {
			s.pipeBytes = tmux.PipeBytes
		}
	}
	if s.capturePane == nil && deps.PipeOutput == nil && deps.PipeBytes == nil {
		s.capturePane = tmux.CapturePaneContext
	}
	if s.sendKeys == nil {
		s.sendKeys = tmux.SendKeysContext
	}
	if s.sendRawKeys == nil {
		s.sendRawKeys = tmux.SendRawKeysContext
	}
	if s.resizePane == nil {
		s.resizePane = tmux.ResizePaneContext
	}
	if s.isSessionAlive == nil {
		s.isSessionAlive = tmux.SessionExistsContext
	}
	if s.isWindowAlive == nil {
		s.isWindowAlive = tmux.IsWindowAliveContext
	}
	if s.isPaneIdle == nil && deps.IsSessionAlive == nil && deps.IsWindowAlive == nil {
		s.isPaneIdle = tmux.IsPaneIdleContext
	}
	if s.ledger != nil {
		s.ledger.SetOnChange(s.broadcastLedgerChange)
	}
	if s.manager != nil {
		s.configureProjectLedgerCallbacks()
	}
	if s.secret == "" && !s.authDisabled {
		log.Printf("web: bearer auth is not configured; set Secret or AuthDisabled=true explicitly")
	}
	s.routes()
	return s
}

func (s *Server) configureProjectLedgerCallbacks() {
	if s.manager == nil {
		return
	}
	for _, info := range s.manager.Projects() {
		project, err := s.manager.Project(info.Slug)
		if err == nil && project.Ledger != nil {
			project.Ledger.SetOnChange(s.projectLedgerChangeCallback(info.Slug))
		}
	}
}

func (s *Server) projectLedgerChangeCallback(slug string) func() {
	return func() {
		s.broadcastLedgerChangeForScope(slug)
	}
}

// Triggerer starts or wakes the orchestrator after a web mutation.
type Triggerer interface {
	Trigger(ctx context.Context, reason string) error
}

// WorkerCleaner removes runtime resources for a task-owned worker.
type WorkerCleaner interface {
	CleanupWorker(ctx context.Context, task ledger.Task) error
}

type cleanupDependencies struct {
	cfg     *config.Config
	session string
}

type defaultWorkerCleaner struct {
	deps     func() cleanupDependencies
	registry *worker.Registry
}

func workerCleanerFromDeps(server *Server, deps Deps) WorkerCleaner {
	if deps.Cleaner != nil {
		return deps.Cleaner
	}
	return defaultWorkerCleaner{
		deps: func() cleanupDependencies {
			cfg, _ := server.configSnapshot()
			return cleanupDependencies{cfg: cfg, session: deps.Session}
		},
		registry: deps.Registry,
	}
}

func (c defaultWorkerCleaner) CleanupWorker(ctx context.Context, task ledger.Task) error {
	deps := c.deps()
	if deps.cfg == nil {
		return fmt.Errorf("config is not configured")
	}
	workerID := task.WorkerID
	if workerID == "" {
		workerID = "worker-" + task.ID
	}
	var errs []error
	if deps.session == "" {
		log.Printf("warn: web cleanup skipping tmux window for %s: session is not configured", workerID)
	}
	if deps.session != "" {
		if err := tmux.KillWindowContext(ctx, deps.session, workerID); err != nil {
			if !isMissingTmuxWindowError(err) {
				errs = append(errs, fmt.Errorf("kill worker window: %w", err))
			}
		}
	}
	repoPath, err := filepath.Abs(deps.cfg.Project.RepoPath)
	if err != nil {
		errs = append(errs, fmt.Errorf("resolve repo path: %w", err))
		repoPath = deps.cfg.Project.RepoPath
	}
	worktreePath, err := config.ProjectWorktreePath(deps.cfg.Project, task.ID)
	if err != nil {
		errs = append(errs, fmt.Errorf("resolve worktree path: %w", err))
	} else {
		if err := worktree.RemoveContext(ctx, repoPath, worktreePath); err != nil {
			if !isMissingWorktreeError(err) {
				errs = append(errs, fmt.Errorf("remove worktree: %w", err))
			}
		}
	}
	if task.Branch != "" {
		if err := worktree.DeleteTaskBranchIfSafeContext(ctx, repoPath, task.Branch, task.ID); err != nil {
			if errors.Is(err, worktree.ErrUnsafeBranchDelete) {
				log.Printf("web cleanup skipped unsafe branch delete for task %s: %v", task.ID, err)
			} else if errors.Is(err, worktree.ErrOriginUnavailable) {
				log.Printf("web cleanup skipped branch delete for task %s (origin unavailable): %v", task.ID, err)
			} else if !isMissingBranchError(err) {
				errs = append(errs, fmt.Errorf("delete branch: %w", err))
			}
		}
	}
	if c.registry != nil && workerID != "" {
		c.registry.Remove(workerID)
	}
	return errors.Join(errs...)
}

func isMissingTmuxWindowError(err error) bool {
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "can't find window") ||
		strings.Contains(msg, "can't find pane") ||
		strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "no such session")
}

func isMissingWorktreeError(err error) bool {
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "is not a working tree") ||
		strings.Contains(msg, "not a working tree")
}

func isMissingBranchError(err error) bool {
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "branch") && strings.Contains(msg, "not found")
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
	if hdr == "" && strings.HasPrefix(r.URL.Path, "/ws/") {
		if token := r.URL.Query().Get("token"); token != "" {
			hdr = "Bearer " + token
		}
	}
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
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
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
	s.mux.HandleFunc("/api/projects", s.handleProjects)
	s.mux.HandleFunc("/api/projects/", s.handleProjectRoute)
	s.mux.HandleFunc("/api/tasks", s.handleTasks)
	s.mux.HandleFunc("/api/tasks/", s.handleTaskByID)
	s.mux.HandleFunc("/api/workers", s.handleWorkers)
	s.mux.HandleFunc("/api/workers/", s.handleWorkerRoute)
	s.mux.HandleFunc("/api/harnesses", s.handleHarnesses)
	s.mux.HandleFunc("/api/config", s.handleConfig)
	s.mux.HandleFunc("/ws/worker/", s.handleWorkerLogWS)
	s.mux.HandleFunc("/ws/orchestrator", s.handleOrchestratorLogWS)
	s.mux.HandleFunc("/ws/ledger", s.handleLedgerWS)
	s.mux.HandleFunc("/ws/projects/", s.handleProjectWS)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.manager != nil {
			projects := s.manager.Projects()
			out := make([]projectResponse, len(projects))
			for i, info := range projects {
				out[i] = projectResponse{
					Slug:     info.Slug,
					RepoPath: info.RepoPath,
				}
				if project, err := s.manager.Project(info.Slug); err == nil {
					out[i].Session = project.Session
				}
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		cfg, ok := s.configSnapshot()
		if !ok {
			writeError(w, http.StatusInternalServerError, "config is not configured")
			return
		}
		session := s.tmuxSession
		if session == "" {
			session = cfg.Runtime.TmuxSession
		}
		writeJSON(w, http.StatusOK, []projectResponse{{
			Slug:     cfg.Project.Slug,
			RepoPath: cfg.Project.RepoPath,
			Session:  session,
		}})
	case http.MethodPost:
		s.createProject(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	if s.configPath == "" {
		writeError(w, http.StatusInternalServerError, "config path is not configured")
		return
	}
	var req projectCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	slug := strings.TrimSpace(req.Slug)
	repoPath := strings.TrimSpace(req.RepoPath)
	if slug == "" || repoPath == "" {
		writeError(w, http.StatusBadRequest, "project slug and repository path are required")
		return
	}
	if err := config.ValidateProjectSlug(slug); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("project slug %q: %v", slug, err))
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	raw, err := config.LoadRaw(s.configPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load config")
		return
	}
	if raw.Projects == nil {
		raw.Projects = make(map[string]config.ProjectConfig)
	}
	if _, exists := raw.Projects[slug]; exists {
		writeError(w, http.StatusConflict, "project already exists")
		return
	}
	worktreeBase := strings.TrimSpace(req.WorktreeBase)
	if worktreeBase == "" {
		worktreeBase = raw.Runtime.WorktreeBase
	}
	raw.Projects[slug] = config.ProjectConfig{
		Slug:              slug,
		RepoPath:          repoPath,
		WorktreeBase:      worktreeBase,
		LedgerPath:        strings.TrimSpace(req.LedgerPath),
		ValidationCommand: strings.TrimSpace(req.ValidationCommand),
	}
	updated, err := s.saveReloadConfigLocked(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, configResponseFromConfig(updated).Projects[slug])
}

func (s *Server) handleProjectRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/projects/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete {
		s.deleteProject(w, r, parts[0])
		return
	}
	if len(parts) < 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "project route not found")
		return
	}
	projectServer, err := s.projectServer(parts[0])
	if err != nil {
		if errors.Is(err, runtimepkg.ErrProjectNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "project is not configured")
		return
	}
	switch parts[1] {
	case "tasks":
		if len(parts) == 2 {
			projectServer.handleTasks(w, r)
			return
		}
		if len(parts) == 3 {
			switch r.Method {
			case http.MethodPatch:
				projectServer.updateTask(w, r, parts[2])
			case http.MethodDelete:
				projectServer.deleteTask(w, r, parts[2])
			default:
				methodNotAllowed(w, http.MethodPatch, http.MethodDelete)
			}
			return
		}
	case "workers":
		if len(parts) == 2 {
			projectServer.handleWorkers(w, r)
			return
		}
		if len(parts) == 4 && parts[3] == "followup" {
			projectServer.sendWorkerFollowup(w, r, parts[2])
			return
		}
	case "orchestrator":
		if len(parts) == 3 && parts[2] == "input" {
			projectServer.sendOrchestratorInput(w, r)
			return
		}
	}
	writeError(w, http.StatusNotFound, "project route not found")
}

func (s *Server) deleteProject(w http.ResponseWriter, _ *http.Request, slug string) {
	if s.configPath == "" {
		writeError(w, http.StatusInternalServerError, "config path is not configured")
		return
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if s.manager != nil {
		project, err := s.manager.Project(slug)
		if err == nil && project.Registry != nil && len(project.Registry.All()) > 0 {
			writeError(w, http.StatusConflict, "project has active workers")
			return
		}
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	raw, err := config.LoadRaw(s.configPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load config")
		return
	}
	if raw.Projects == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if _, exists := raw.Projects[slug]; !exists {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	delete(raw.Projects, slug)
	if raw.Project.Slug == slug || len(raw.Projects) == 0 {
		raw.Project = config.ProjectConfig{}
	}
	if _, err := s.saveReloadConfigLocked(raw); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) projectServer(slug string) (*Server, error) {
	if s.manager == nil {
		cfg, ok := s.configSnapshot()
		if !ok || cfg.Project.Slug != slug {
			return nil, fmt.Errorf("%w: %s", runtimepkg.ErrProjectNotFound, slug)
		}
		return s, nil
	}
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	project, err := s.manager.Project(slug)
	if err != nil {
		return nil, err
	}
	out := &Server{
		cfg:             project.Config,
		ledger:          project.Ledger,
		manager:         s.manager,
		registry:        project.Registry,
		trigger:         project.Orchestrator,
		pipeOutput:      s.pipeOutput,
		pipeBytes:       s.pipeBytes,
		capturePane:     s.capturePane,
		sendKeys:        s.sendKeys,
		sendRawKeys:     s.sendRawKeys,
		resizePane:      s.resizePane,
		isSessionAlive:  s.isSessionAlive,
		isWindowAlive:   s.isWindowAlive,
		isPaneIdle:      s.isPaneIdle,
		tmuxSession:     project.Session,
		projectScoped:   true,
		pendingTriggers: s.pendingTriggers,
		secret:          s.secret,
		authDisabled:    s.authDisabled,
		allowedOrigins:  s.allowedOrigins,
		harnesses:       s.harnesses,
		tmuxStreams:     s.tmuxStreams,
		startLocks:      s.startLocks,
	}
	out.cleaner = defaultWorkerCleaner{
		deps: func() cleanupDependencies {
			return cleanupDependencies{cfg: project.Config, session: project.Session}
		},
		registry: project.Registry,
	}
	return out, nil
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
	case http.MethodDelete:
		s.deleteTask(w, r, id)
	default:
		methodNotAllowed(w, http.MethodPatch, http.MethodDelete)
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
			s.writeTaskCreateResponse(w, r, http.StatusOK, existing, triggerIfPending)
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
					s.writeTaskCreateResponse(w, r, http.StatusOK, existing, triggerIfPending)
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
	s.writeTaskCreateResponse(w, r, http.StatusCreated, task, triggerAlways)
}

type triggerPolicy int

const (
	triggerAlways triggerPolicy = iota
	triggerIfPending
)

type pendingTriggerSet struct {
	mu    sync.Mutex
	tasks map[string]triggerState
}

type triggerState int

const (
	triggerStatePending triggerState = iota + 1
	triggerStateInFlight
)

func (p *pendingTriggerSet) begin(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = make(map[string]triggerState)
	}
	p.tasks[key] = triggerStateInFlight
}

func (p *pendingTriggerSet) beginPending(key string) bool {
	if p == nil || key == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = make(map[string]triggerState)
	}
	if p.tasks[key] != triggerStatePending {
		return false
	}
	p.tasks[key] = triggerStateInFlight
	return true
}

func (p *pendingTriggerSet) mark(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tasks == nil {
		p.tasks = make(map[string]triggerState)
	}
	p.tasks[key] = triggerStatePending
}

func (p *pendingTriggerSet) clear(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tasks, key)
}

func (s *Server) writeTaskCreateResponse(w http.ResponseWriter, r *http.Request, status int, task ledger.Task, policy triggerPolicy) {
	resp := taskCreateResponse{Task: taskResponseFromLedger(task)}
	if s.trigger != nil {
		key := s.taskTriggerKey(task.ID)
		switch policy {
		case triggerAlways:
			s.pendingTriggers.begin(key)
		case triggerIfPending:
			if !s.pendingTriggers.beginPending(key) {
				writeJSON(w, status, resp)
				return
			}
		}
		if key == "" {
			writeJSON(w, status, resp)
			return
		}
		triggered := false
		defer func() {
			if !triggered {
				s.pendingTriggers.mark(key)
			}
		}()
		if err := s.trigger.Trigger(context.WithoutCancel(r.Context()), "task created: "+task.ID); err != nil {
			log.Printf("web: orchestrator trigger after task create failed: %v", err)
			resp.TriggerError = "orchestrator trigger failed"
			status = http.StatusAccepted
		} else {
			triggered = true
			s.pendingTriggers.clear(key)
			resp.OrchestratorTriggered = true
		}
	}
	writeJSON(w, status, resp)
}

func (s *Server) taskTriggerKey(taskID string) string {
	if taskID == "" {
		return ""
	}
	cfg, ok := s.configSnapshot()
	if !ok || cfg.Project.Slug == "" {
		return taskID
	}
	return cfg.Project.Slug + "\x00" + taskID
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
		if errors.Is(err, errTaskDeletingConflict) {
			writeError(w, http.StatusConflict, "task delete is in progress")
			return
		}
		writeError(w, http.StatusInternalServerError, "update task")
		return
	}
	updated := taskSnapshotWithFields(prev, fields)
	updated.UpdatedAt = ""
	writeJSON(w, http.StatusOK, taskResponseFromLedger(updated))
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request, id string) {
	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, "ledger is not configured")
		return
	}
	task, marker, needsCleanup, err := s.prepareTaskForDelete(id)
	if err != nil {
		if errors.Is(err, ledger.ErrTaskNotFound) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		if errors.Is(err, ledger.ErrTaskDeleteInProgress) {
			writeJSON(w, http.StatusAccepted, taskDeleteResponse{
				Deleted:       taskResponseFromLedger(task),
				DeletePending: true,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "delete task")
		return
	}
	resp := taskDeleteResponse{Deleted: taskResponseFromLedger(task)}
	if needsCleanup {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), deleteCleanupTimeout)
		defer cancel()
		if err := s.cleaner.CleanupWorker(cleanupCtx, task); err != nil {
			log.Printf("web: cleanup after task delete failed: %v", err)
			restored, restoreErr := s.ledger.RestoreTaskSnapshotIfCurrent(task, marker)
			if restoreErr != nil {
				log.Printf("web: restore after cleanup failure failed: %v", restoreErr)
				writeError(w, http.StatusInternalServerError, "restore task after cleanup failure")
				return
			}
			resp.CleanupError = "worker cleanup failed"
			if !restored {
				resp.DeletePending = true
			}
			writeJSON(w, http.StatusAccepted, resp)
			return
		}
		resp.WorkerCleaned = true
	} else {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	deleted, err := s.ledger.DeleteTaskIfCurrent(marker)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete task")
		return
	}
	if !deleted {
		resp.DeletePending = true
		writeJSON(w, http.StatusAccepted, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) prepareTaskForDelete(id string) (ledger.Task, ledger.Task, bool, error) {
	now := time.Now()
	return s.ledger.PrepareDeleteReturnPrev(id, func(task ledger.Task) bool {
		return taskNeedsDeleteCleanup(task, now)
	})
}

func taskNeedsDeleteCleanup(task ledger.Task, now time.Time) bool {
	if task.Status == "deleting" {
		return deletingMarkerExpired(task.UpdatedAt, now)
	}
	return task.Status == "in_progress" ||
		task.Status == "blocked" ||
		task.WorkerID != "" ||
		task.Branch != ""
}

func deletingMarkerExpired(updatedAt string, now time.Time) bool {
	if updatedAt == "" {
		return true
	}
	ts, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return true
	}
	return !ts.After(now.Add(-deleteCleanupLease))
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

func (s *Server) handleWorkerRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/workers/")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] == "followup" {
		s.sendWorkerFollowup(w, r, parts[0])
		return
	}
	writeError(w, http.StatusNotFound, "worker route not found")
}

func (s *Server) sendWorkerFollowup(w http.ResponseWriter, r *http.Request, workerID string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		writeError(w, http.StatusBadRequest, "worker_id is required")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, "ledger is not configured")
		return
	}
	var req workerFollowupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	message, err := normalizeFollowupMessage(req.Message)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	task, err := s.activeTaskForWorker(workerID)
	if err != nil {
		if errors.Is(err, errWorkerNotActive) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "load worker task")
		return
	}
	session, err := s.tmuxSessionName()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	aliveCtx, cancel := context.WithTimeout(r.Context(), followupTmuxOperationTimeout)
	alive, err := s.isWindowAlive(aliveCtx, session, workerID)
	cancel()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check worker tmux window")
		return
	}
	if !alive {
		writeError(w, http.StatusNotFound, "worker tmux window not found")
		return
	}
	task, err = s.activeTaskForWorker(workerID)
	if err != nil {
		if errors.Is(err, errWorkerNotActive) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "load worker task")
		return
	}
	sendCtx, cancel := context.WithTimeout(r.Context(), followupTmuxOperationTimeout)
	defer cancel()
	if err := s.sendKeys(sendCtx, session, workerID, message); err != nil {
		writeError(w, http.StatusInternalServerError, "send worker followup")
		return
	}
	writeJSON(w, http.StatusOK, workerFollowupResponse{
		Sent:     true,
		TaskID:   task.ID,
		WorkerID: workerID,
		Session:  session,
		Window:   workerID,
	})
}

func (s *Server) sendOrchestratorInput(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	window, err := s.orchestratorWindowName()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req orchestratorInputRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	message, err := normalizeFollowupMessage(req.Message)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	session, err := s.tmuxSessionName()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	aliveCtx, cancel := context.WithTimeout(r.Context(), followupTmuxOperationTimeout)
	sessionAlive, err := s.isSessionAlive(aliveCtx, session)
	cancel()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check tmux session")
		return
	}
	if !sessionAlive {
		writeError(w, http.StatusNotFound, "tmux session not found")
		return
	}
	aliveCtx, cancel = context.WithTimeout(r.Context(), followupTmuxOperationTimeout)
	windowAlive, err := s.isWindowAlive(aliveCtx, session, window)
	cancel()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check orchestrator tmux window")
		return
	}
	if !windowAlive {
		writeError(w, http.StatusNotFound, "orchestrator tmux window not found")
		return
	}
	sendCtx, cancel := context.WithTimeout(r.Context(), followupTmuxOperationTimeout)
	defer cancel()
	if err := s.sendKeys(sendCtx, session, window, message); err != nil {
		writeError(w, http.StatusInternalServerError, "send orchestrator input")
		return
	}
	writeJSON(w, http.StatusOK, orchestratorInputResponse{
		Sent:    true,
		Session: session,
		Window:  window,
	})
}

func (s *Server) orchestratorWindowName() (string, error) {
	cfg, ok := s.configSnapshot()
	if !ok {
		return "", fmt.Errorf("config is not configured")
	}
	slug := strings.TrimSpace(cfg.Project.Slug)
	if s.projectScoped {
		if slug == "" {
			return "", fmt.Errorf("project is not configured")
		}
		return slug + "-orchestrator", nil
	}
	return "orchestrator", nil
}

func (s *Server) activeTaskForWorker(workerID string) (ledger.Task, error) {
	tasks, err := s.ledger.Load()
	if err != nil {
		return ledger.Task{}, err
	}
	for _, task := range tasks {
		if task.WorkerID != workerID {
			continue
		}
		if task.Status != "in_progress" && task.Status != "blocked" {
			return ledger.Task{}, fmt.Errorf("%w: task %s status is %q", errWorkerNotActive, task.ID, task.Status)
		}
		return task, nil
	}
	return ledger.Task{}, fmt.Errorf("%w: no active task found with worker_id %q", errWorkerNotActive, workerID)
}

func normalizeFollowupMessage(input string) (string, error) {
	message := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(strings.Trim(input, "\r\n"))
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("message is required")
	}
	if len(message) > maxFollowupMessageBytes {
		return "", fmt.Errorf("message is too large")
	}
	for _, r := range message {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", fmt.Errorf("message contains unsupported control characters")
		}
	}
	return message, nil
}

func (s *Server) tmuxSessionName() (string, error) {
	cfg, ok := s.configSnapshot()
	if !ok {
		return "", fmt.Errorf("config is not configured")
	}
	session := s.tmuxSession
	if session == "" {
		session = cfg.Runtime.TmuxSession
	}
	if session == "" {
		session = cfg.Project.Slug
	}
	if session == "" {
		return "", fmt.Errorf("tmux session is not configured")
	}
	return session, nil
}

func (s *Server) handleHarnesses(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	harnesses, ok := s.harnessSnapshot()
	if !ok {
		writeError(w, http.StatusInternalServerError, "config is not configured")
		return
	}
	writeJSON(w, http.StatusOK, harnesses)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getConfig(w, r)
	case http.MethodPatch:
		s.patchConfig(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPatch)
	}
}

func (s *Server) configSnapshot() (*config.Config, bool) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.cfg == nil {
		return nil, false
	}
	return config.Clone(s.cfg), true
}

func (s *Server) harnessSnapshot() ([]harnessResponse, bool) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.cfg == nil {
		return nil, false
	}
	return append([]harnessResponse(nil), s.harnesses...), true
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg, ok := s.configSnapshot()
	if !ok {
		writeError(w, http.StatusInternalServerError, "config is not configured")
		return
	}
	writeJSON(w, http.StatusOK, configResponseFromConfig(cfg))
}

func (s *Server) patchConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.configSnapshot(); !ok {
		writeError(w, http.StatusInternalServerError, "config is not configured")
		return
	}
	if s.configPath == "" {
		writeError(w, http.StatusInternalServerError, "config path is not configured")
		return
	}
	var req configPatchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	raw, err := config.LoadRaw(s.configPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load config")
		return
	}
	if err := applyConfigPatch(raw, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.saveReloadConfigLocked(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, configResponseFromConfig(updated))
}

func (s *Server) saveReloadConfigLocked(raw *config.Config) (*config.Config, error) {
	if err := config.Save(s.configPath, raw); err != nil {
		return nil, err
	}
	updated, err := config.Load(s.configPath)
	if err != nil {
		return nil, fmt.Errorf("reload config: %w", err)
	}
	if s.manager != nil {
		if err := s.manager.Reload(updated, s.projectLedgerChangeCallback); err != nil {
			return nil, err
		}
	}
	s.cfg = config.Clone(updated)
	s.harnesses = harnessResponsesFromConfig(s.cfg)
	return updated, nil
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

type projectResponse struct {
	Slug     string `json:"slug"`
	RepoPath string `json:"repo_path"`
	Session  string `json:"session,omitempty"`
}

type taskCreateResponse struct {
	Task                  taskResponse `json:"task"`
	OrchestratorTriggered bool         `json:"orchestrator_triggered"`
	TriggerError          string       `json:"trigger_error,omitempty"`
}

type taskDeleteResponse struct {
	Deleted       taskResponse `json:"deleted"`
	WorkerCleaned bool         `json:"worker_cleaned,omitempty"`
	CleanupError  string       `json:"cleanup_error,omitempty"`
	DeletePending bool         `json:"delete_pending,omitempty"`
}

type workerFollowupRequest struct {
	Message string `json:"message"`
}

type workerFollowupResponse struct {
	Sent     bool   `json:"sent"`
	TaskID   string `json:"task_id"`
	WorkerID string `json:"worker_id"`
	Session  string `json:"session"`
	Window   string `json:"window"`
}

type orchestratorInputRequest struct {
	Message string `json:"message"`
}

type orchestratorInputResponse struct {
	Sent    bool   `json:"sent"`
	Session string `json:"session"`
	Window  string `json:"window"`
}

type taskMutationRequest struct {
	IdempotencyKey *string   `json:"idempotency_key"`
	Request        *string   `json:"request"`
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
	if err := validateTaskMutation(task); err != nil {
		return ledger.Task{}, err
	}
	if strings.TrimSpace(task.Title) == "" && strings.TrimSpace(task.Body) != "" {
		task.Title = "Natural language intake"
	}
	if strings.TrimSpace(task.Title) == "" && strings.TrimSpace(task.Body) == "" {
		return ledger.Task{}, fmt.Errorf("title or body (or request) is required")
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
var errTaskDeletingConflict = errors.New("task delete is in progress")
var errWorkerNotActive = errors.New("worker is not active")

func validateTaskMutation(task ledger.Task) error {
	return validateTaskMutationFields(map[string]any{
		"status":          task.Status,
		"allowed_files":   task.AllowedFiles,
		"forbidden_files": task.ForbiddenFiles,
	})
}

func validateTaskMutationFields(fields map[string]any) error {
	if status, ok := fields["status"].(string); ok && !validTaskStatus(status) {
		return fmt.Errorf("status must be one of: unstarted, in_progress, blocked, completed, split")
	}
	if allowedFiles, ok := fields["allowed_files"].([]string); ok {
		if err := ledger.ValidatePaths(allowedFiles); err != nil {
			return fmt.Errorf("allowed_files: %w", err)
		}
	}
	if forbiddenFiles, ok := fields["forbidden_files"].([]string); ok {
		if err := ledger.ValidatePaths(forbiddenFiles); err != nil {
			return fmt.Errorf("forbidden_files: %w", err)
		}
	}
	return nil
}

func validTaskStatus(status string) bool {
	switch status {
	case "unstarted", "in_progress", "blocked", "completed", "split":
		return true
	default:
		return false
	}
}

func validateTaskUpdate(current ledger.Task, fields map[string]any) error {
	if current.Status == "deleting" {
		return errTaskDeletingConflict
	}
	if status, ok := fields["status"].(string); ok && strings.TrimSpace(status) == "" {
		return fmt.Errorf("%w: status cannot be empty", errInvalidTaskMutation)
	}
	if err := validateTaskMutationFields(fields); err != nil {
		return fmt.Errorf("%w: %w", errInvalidTaskMutation, err)
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
		body := strings.Trim(*req.Body, "\n")
		if strings.TrimSpace(body) == "" && req.Request != nil && strings.TrimSpace(*req.Request) != "" {
			body = strings.Trim(*req.Request, "\n")
		}
		fields["body"] = body
	} else if req.Request != nil && strings.TrimSpace(*req.Request) != "" {
		fields["body"] = strings.Trim(*req.Request, "\n")
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
	Runtime         runtimeConfigResponse            `json:"runtime"`
	Orchestrator    orchestratorConfigResponse       `json:"orchestrator"`
	WorkerHarnesses []string                         `json:"worker_harnesses"`
	Harnesses       map[string]harnessConfigResponse `json:"harnesses"`
	GitHub          githubConfigResponse             `json:"github"`
	Projects        map[string]projectConfigResponse `json:"projects"`
}

type configPatchRequest struct {
	Project         *projectConfigPatch           `json:"project"`
	Server          *serverConfigPatch            `json:"server"`
	Runtime         *runtimeConfigPatch           `json:"runtime"`
	Orchestrator    *orchestratorConfigPatch      `json:"orchestrator"`
	WorkerHarnesses *[]string                     `json:"worker_harnesses"`
	Harnesses       map[string]harnessConfigPatch `json:"harnesses"`
	GitHub          *githubConfigPatch            `json:"github"`
	Projects        map[string]projectConfigPatch `json:"projects"`
}

type projectConfigResponse struct {
	Slug              string                     `json:"slug"`
	RepoPath          string                     `json:"repo_path"`
	WorktreeBase      string                     `json:"worktree_base"`
	LedgerPath        string                     `json:"ledger_path,omitempty"`
	ValidationCommand string                     `json:"validation_command,omitempty"`
	Orchestrator      orchestratorConfigResponse `json:"orchestrator,omitempty"`
	GitHub            githubConfigResponse       `json:"github,omitempty"`
}

type projectConfigPatch struct {
	Slug              *string `json:"slug"`
	RepoPath          *string `json:"repo_path"`
	WorktreeBase      *string `json:"worktree_base"`
	LedgerPath        *string `json:"ledger_path"`
	ValidationCommand *string `json:"validation_command"`
}

type projectCreateRequest struct {
	Slug              string `json:"slug"`
	RepoPath          string `json:"repo_path"`
	WorktreeBase      string `json:"worktree_base"`
	LedgerPath        string `json:"ledger_path"`
	ValidationCommand string `json:"validation_command"`
}

type runtimeConfigResponse struct {
	TmuxSession  string `json:"tmux_session"`
	WorktreeBase string `json:"worktree_base"`
}

type runtimeConfigPatch struct {
	TmuxSession  *string `json:"tmux_session"`
	WorktreeBase *string `json:"worktree_base"`
}

type serverConfigResponse struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type serverConfigPatch struct {
	Host *string `json:"host"`
	Port *int    `json:"port"`
}

type orchestratorConfigResponse struct {
	Harness           string `json:"harness"`
	HeartbeatInterval string `json:"heartbeat_interval"`
	Timeout           string `json:"timeout"`
}

type orchestratorConfigPatch struct {
	Harness           *string `json:"harness"`
	HeartbeatInterval *string `json:"heartbeat_interval"`
	Timeout           *string `json:"timeout"`
}

type githubConfigResponse struct {
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
}

type githubConfigPatch struct {
	Owner *string `json:"owner"`
	Repo  *string `json:"repo"`
}

type harnessConfigResponse struct {
	Command string `json:"command"`
}

type harnessConfigPatch struct {
	Command *string `json:"command"`
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
	workerHarnesses := append([]string{}, cfg.WorkerHarnesses...)
	harnesses := make(map[string]harnessConfigResponse, len(cfg.Harnesses))
	for name, h := range cfg.Harnesses {
		harnesses[name] = harnessConfigResponse{Command: h.Command}
	}
	projects := make(map[string]projectConfigResponse, len(cfg.Projects))
	for slug, project := range cfg.Projects {
		projects[slug] = projectConfigResponseFromConfig(project)
	}
	return configResponse{
		Project: projectConfigResponseFromConfig(cfg.Project),
		Server: serverConfigResponse{
			Host: cfg.Server.Host,
			Port: cfg.Server.Port,
		},
		Runtime: runtimeConfigResponse{
			TmuxSession:  cfg.Runtime.TmuxSession,
			WorktreeBase: cfg.Runtime.WorktreeBase,
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
		Projects: projects,
	}
}

func projectConfigResponseFromConfig(project config.ProjectConfig) projectConfigResponse {
	return projectConfigResponse{
		Slug:         project.Slug,
		RepoPath:     project.RepoPath,
		WorktreeBase: project.WorktreeBase,
		LedgerPath:   project.LedgerPath,
		Orchestrator: orchestratorConfigResponse{
			Harness:           project.Orchestrator.Harness,
			HeartbeatInterval: project.Orchestrator.HeartbeatInterval.String(),
			Timeout:           project.Orchestrator.Timeout.String(),
		},
		GitHub: githubConfigResponse{
			Owner: project.GitHub.Owner,
			Repo:  project.GitHub.Repo,
		},
	}
}

func applyConfigPatch(cfg *config.Config, req configPatchRequest) error {
	changed := false
	if req.Project != nil {
		if req.Project.Slug != nil {
			slug := strings.TrimSpace(*req.Project.Slug)
			if err := config.ValidateProjectSlug(slug); err != nil {
				return fmt.Errorf("project.slug %q: %w", slug, err)
			}
			cfg.Project.Slug = slug
			changed = true
		}
		if req.Project.RepoPath != nil {
			cfg.Project.RepoPath = strings.TrimSpace(*req.Project.RepoPath)
			changed = true
		}
		if req.Project.WorktreeBase != nil {
			cfg.Project.WorktreeBase = strings.TrimSpace(*req.Project.WorktreeBase)
			changed = true
		}
		if req.Project.LedgerPath != nil {
			cfg.Project.LedgerPath = strings.TrimSpace(*req.Project.LedgerPath)
			changed = true
		}
		if req.Project.ValidationCommand != nil {
			cfg.Project.ValidationCommand = strings.TrimSpace(*req.Project.ValidationCommand)
			changed = true
		}
	}
	if req.Server != nil {
		if req.Server.Host != nil {
			cfg.Server.Host = strings.TrimSpace(*req.Server.Host)
			changed = true
		}
		if req.Server.Port != nil {
			cfg.Server.Port = *req.Server.Port
			changed = true
		}
	}
	if req.Runtime != nil {
		if req.Runtime.TmuxSession != nil {
			cfg.Runtime.TmuxSession = strings.TrimSpace(*req.Runtime.TmuxSession)
			changed = true
		}
		if req.Runtime.WorktreeBase != nil {
			cfg.Runtime.WorktreeBase = strings.TrimSpace(*req.Runtime.WorktreeBase)
			changed = true
		}
	}
	if req.Orchestrator != nil {
		if req.Orchestrator.Harness != nil {
			cfg.Orchestrator.Harness = strings.TrimSpace(*req.Orchestrator.Harness)
			changed = true
		}
		if req.Orchestrator.HeartbeatInterval != nil {
			d, err := time.ParseDuration(strings.TrimSpace(*req.Orchestrator.HeartbeatInterval))
			if err != nil {
				return fmt.Errorf("orchestrator.heartbeat_interval must be a duration")
			}
			cfg.Orchestrator.HeartbeatInterval = d
			changed = true
		}
		if req.Orchestrator.Timeout != nil {
			d, err := time.ParseDuration(strings.TrimSpace(*req.Orchestrator.Timeout))
			if err != nil {
				return fmt.Errorf("orchestrator.timeout must be a duration")
			}
			cfg.Orchestrator.Timeout = d
			changed = true
		}
	}
	if req.WorkerHarnesses != nil {
		cfg.WorkerHarnesses = append([]string{}, *req.WorkerHarnesses...)
		for i := range cfg.WorkerHarnesses {
			cfg.WorkerHarnesses[i] = strings.TrimSpace(cfg.WorkerHarnesses[i])
		}
		changed = true
	}
	if req.Harnesses != nil {
		if cfg.Harnesses == nil {
			cfg.Harnesses = make(map[string]config.HarnessConfig)
		}
		for name, patch := range req.Harnesses {
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("harness name cannot be empty")
			}
			current, ok := cfg.Harnesses[name]
			if !ok {
				return fmt.Errorf("harness %q does not exist", name)
			}
			if patch.Command != nil {
				current.Command = strings.TrimSpace(*patch.Command)
				cfg.Harnesses[name] = current
				changed = true
			}
		}
	}
	if req.GitHub != nil {
		if req.GitHub.Owner != nil {
			cfg.GitHub.Owner = strings.TrimSpace(*req.GitHub.Owner)
			changed = true
		}
		if req.GitHub.Repo != nil {
			cfg.GitHub.Repo = strings.TrimSpace(*req.GitHub.Repo)
			changed = true
		}
	}
	if req.Projects != nil {
		if cfg.Projects == nil {
			cfg.Projects = make(map[string]config.ProjectConfig)
		}
		for slug, patch := range req.Projects {
			slug = strings.TrimSpace(slug)
			if err := config.ValidateProjectSlug(slug); err != nil {
				return fmt.Errorf("project slug %q: %w", slug, err)
			}
			current := cfg.Projects[slug]
			current.Slug = slug
			if patch.Slug != nil {
				nextSlug := strings.TrimSpace(*patch.Slug)
				if err := config.ValidateProjectSlug(nextSlug); err != nil {
					return fmt.Errorf("project slug %q: %w", nextSlug, err)
				}
				if nextSlug != slug {
					delete(cfg.Projects, slug)
					slug = nextSlug
					current.Slug = nextSlug
				}
			}
			if patch.RepoPath != nil {
				current.RepoPath = strings.TrimSpace(*patch.RepoPath)
			}
			if patch.WorktreeBase != nil {
				current.WorktreeBase = strings.TrimSpace(*patch.WorktreeBase)
			}
			if patch.LedgerPath != nil {
				current.LedgerPath = strings.TrimSpace(*patch.LedgerPath)
			}
			if patch.ValidationCommand != nil {
				current.ValidationCommand = strings.TrimSpace(*patch.ValidationCommand)
			}
			cfg.Projects[slug] = current
			changed = true
		}
	}
	if !changed {
		return fmt.Errorf("no config fields provided")
	}
	return nil
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
