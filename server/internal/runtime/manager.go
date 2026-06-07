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
	Slug         string
	Config       *config.Config
	Ledger       *ledger.Ledger
	Registry     *worker.Registry
	GitHub       *githubpkg.Client
	Orchestrator *orchestrator.Orchestrator
	Scheduler    *scheduler.Scheduler
	Session      string
	BaseURL      string
}

type Manager struct {
	cfg      *config.Config
	session  string
	baseURL  string
	mu       sync.RWMutex
	projects map[string]*ProjectRuntime
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
	for slug := range cfg.Projects {
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
		o := orchestrator.NewProject(l, projectCfg, m.session, m.baseURL, slug+"-orchestrator")
		m.projects[slug] = &ProjectRuntime{
			Slug:         slug,
			Config:       projectCfg,
			Ledger:       l,
			Registry:     registry,
			GitHub:       gh,
			Orchestrator: o,
			Scheduler:    scheduler.New(l, o, projectCfg.Orchestrator.HeartbeatInterval),
			Session:      m.session,
			BaseURL:      m.baseURL,
		}
	}
	return m, nil
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, project := range m.projects {
		go func(project *ProjectRuntime) {
			_ = project.Scheduler.Run(ctx)
		}(project)
	}
}

func (p *ProjectRuntime) Close() {
	if p != nil && p.Orchestrator != nil {
		p.Orchestrator.Close()
	}
}

func (m *Manager) Close() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, project := range m.projects {
		project.Close()
	}
}
