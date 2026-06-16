package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/xpadev/ccx-t2/internal/config"
	githubpkg "github.com/xpadev/ccx-t2/internal/github"
	"github.com/xpadev/ccx-t2/internal/ledger"
	"github.com/xpadev/ccx-t2/internal/orchestrator"
	"github.com/xpadev/ccx-t2/internal/scheduler"
	"github.com/xpadev/ccx-t2/internal/worker"
)

var ErrProjectNotFound = errors.New("project not found")

type ProjectInfo struct {
	Slug     string `json:"slug"`
	RepoPath string `json:"repo_path"`
}

type ProjectRuntime struct {
	Slug          string
	Config        *config.Config
	Ledger        *ledger.Ledger
	Registry      *worker.Registry
	GitHub        *githubpkg.Client
	Orchestrator  *orchestrator.Orchestrator
	NotifyTrigger scheduler.Triggerer
	Scheduler     *scheduler.Scheduler
	Session       string
	BaseURL       string
	cancel        context.CancelFunc
}

type Manager struct {
	cfg      *config.Config
	session  string
	baseURL  string
	mu       sync.RWMutex
	projects map[string]*ProjectRuntime
	ctx      context.Context
	started  bool
}

func NewManager(cfg *config.Config, baseURL string) (*Manager, error) {
	if cfg == nil {
		return nil, fmt.Errorf("runtime: config is nil")
	}
	m := &Manager{
		cfg:      config.Clone(cfg),
		session:  cfg.Runtime.TmuxSession,
		baseURL:  baseURL,
		projects: make(map[string]*ProjectRuntime, len(cfg.Projects)),
	}
	projects, err := buildProjects(cfg, m.session, m.baseURL)
	if err != nil {
		return nil, err
	}
	m.projects = projects
	return m, nil
}

func buildProjects(cfg *config.Config, session, baseURL string) (map[string]*ProjectRuntime, error) {
	projects := make(map[string]*ProjectRuntime, len(cfg.Projects))
	for slug := range cfg.Projects {
		project, err := buildProjectRuntime(cfg, slug, session, baseURL)
		if err != nil {
			return nil, err
		}
		projects[slug] = project
	}
	return projects, nil
}

func buildProjectRuntime(cfg *config.Config, slug, session, baseURL string) (*ProjectRuntime, error) {
	projectCfg, ok := config.Project(cfg, slug)
	if !ok {
		return nil, fmt.Errorf("runtime: %w: %s", ErrProjectNotFound, slug)
	}
	archiveDir := filepath.Join(filepath.Dir(projectCfg.Project.LedgerPath), "archive")
	if err := os.MkdirAll(filepath.Dir(projectCfg.Project.LedgerPath), 0o755); err != nil {
		return nil, fmt.Errorf("runtime: project %s ledger dir: %w", slug, err)
	}
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: project %s archive dir: %w", slug, err)
	}
	l := ledger.NewLedger(projectCfg.Project.LedgerPath, archiveDir)
	registry := worker.NewRegistry()
	var gh *githubpkg.Client
	if projectCfg.GitHub.Token != "" || projectCfg.GitHub.Owner != "" || projectCfg.GitHub.Repo != "" {
		client, err := githubpkg.NewClient(projectCfg.GitHub.Token, projectCfg.GitHub.Owner, projectCfg.GitHub.Repo)
		if err != nil {
			return nil, fmt.Errorf("runtime: project %s github: %w", slug, err)
		}
		gh = client
	}
	orchestratorCfg := config.CloneForOrchestratorRuntime(projectCfg)
	workerCfg := config.CloneForWorkerRuntime(projectCfg)
	o := orchestrator.NewProject(l, orchestratorCfg, session, baseURL, slug+"-orchestrator")
	return &ProjectRuntime{
		Slug:          slug,
		Config:        workerCfg,
		Ledger:        l,
		Registry:      registry,
		GitHub:        gh,
		Orchestrator:  o,
		NotifyTrigger: o,
		Scheduler:     scheduler.New(l, o, projectCfg.Orchestrator.HeartbeatInterval),
		Session:       session,
		BaseURL:       baseURL,
	}, nil
}

func (m *Manager) Project(slug string) (*ProjectRuntime, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	project, ok := m.projects[slug]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, slug)
	}
	return project, nil
}

func (m *Manager) Projects() []ProjectInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ProjectInfo, 0, len(m.projects))
	for slug, project := range m.projects {
		out = append(out, ProjectInfo{Slug: slug, RepoPath: project.Config.Project.RepoPath})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx = ctx
	m.started = true
	for _, project := range m.projects {
		project.start(ctx)
	}
}

func (p *ProjectRuntime) Close() {
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	if p.Orchestrator != nil {
		p.Orchestrator.Close()
	}
}

func (p *ProjectRuntime) start(ctx context.Context) {
	if p == nil || p.Scheduler == nil || p.cancel != nil {
		return
	}
	projectCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	go func() {
		_ = p.Scheduler.Run(projectCtx)
	}()
}

func (m *Manager) Reload(cfg *config.Config, onLedgerChange func()) error {
	if cfg == nil {
		return fmt.Errorf("runtime: config is nil")
	}
	nextCfg := config.Clone(cfg)
	projects, err := buildProjects(nextCfg, nextCfg.Runtime.TmuxSession, m.baseURL)
	if err != nil {
		return err
	}
	for _, project := range projects {
		if project.Ledger != nil && onLedgerChange != nil {
			project.Ledger.SetOnChange(onLedgerChange)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, project := range m.projects {
		project.Close()
	}
	m.cfg = nextCfg
	m.session = nextCfg.Runtime.TmuxSession
	m.projects = projects
	if m.started && m.ctx != nil {
		for _, project := range m.projects {
			project.start(m.ctx)
		}
	}
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, project := range m.projects {
		project.Close()
	}
	m.started = false
}
