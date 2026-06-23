package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
)

func TestManagerUsesWorkerMCPSecretForProjectRuntime(t *testing.T) {
	cfg := testManagerConfig(t, "orchestrator")

	manager, err := NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	project, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got := project.Config.Server.McpSecret; got != "worker" {
		t.Fatalf("project mcp_secret = %q, want worker", got)
	}
	if got := cfg.Server.McpSecret; got != "legacy" {
		t.Fatalf("source mcp_secret = %q, want legacy", got)
	}
}

func TestManagerReloadUsesWorkerMCPSecretForProjectRuntime(t *testing.T) {
	cfg := testManagerConfig(t, "orchestrator-one")
	manager, err := NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	reloaded := testManagerConfig(t, "orchestrator-two")
	if err := manager.Reload(reloaded, nil); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	project, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got := project.Config.Server.McpSecret; got != "worker" {
		t.Fatalf("project mcp_secret after reload = %q, want worker", got)
	}
}

func TestManagerReloadInstallsLedgerCallbacksBeforeExposingProjects(t *testing.T) {
	cfg := testManagerConfig(t, "orchestrator-one")
	manager, err := NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)

	oldProject, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project before reload: %v", err)
	}
	calls := make(chan string, 1)
	reloaded := testManagerConfig(t, "orchestrator-two")
	if err := manager.Reload(reloaded, func(slug string) func() {
		if got := manager.projects[slug]; got != oldProject {
			t.Fatalf("callback factory saw exposed project %#v, want old project %#v", got, oldProject)
		}
		return func() {
			calls <- slug
		}
	}); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	newProject, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project after reload: %v", err)
	}
	if newProject == oldProject {
		t.Fatal("Project after reload reused old project runtime")
	}
	if err := newProject.Ledger.Add(ledger.Task{ID: "task-20260101-0001", Title: "Changed", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	select {
	case got := <-calls:
		if got != "alpha" {
			t.Fatalf("callback slug = %q, want alpha", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ledger callback")
	}
}

func TestRuntimeConfigClonesUseRoleSpecificMCPSecrets(t *testing.T) {
	cfg := testManagerConfig(t, "orchestrator")

	orchestratorCfg := config.CloneForOrchestratorRuntime(cfg)
	if got := orchestratorCfg.Server.McpSecret; got != "orchestrator" {
		t.Fatalf("orchestrator runtime mcp_secret = %q, want orchestrator", got)
	}

	workerCfg := config.CloneForWorkerRuntime(cfg)
	if got := workerCfg.Server.McpSecret; got != "worker" {
		t.Fatalf("worker runtime mcp_secret = %q, want worker", got)
	}

	if got := cfg.Server.McpSecret; got != "legacy" {
		t.Fatalf("source mcp_secret = %q, want legacy", got)
	}
}

func testManagerConfig(t *testing.T, orchestratorSecret string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Server: config.ServerConfig{
			Host:               "127.0.0.1",
			Port:               8080,
			McpSecret:          "legacy",
			OrchestratorSecret: orchestratorSecret,
			WorkerSecret:       "worker",
			WebAdminSecret:     "web",
		},
		Runtime: config.RuntimeConfig{
			TmuxSession:  "ccx-test",
			WorktreeBase: filepath.Join(dir, "worktrees"),
		},
		Orchestrator: config.OrchestratorConfig{
			HeartbeatInterval: time.Minute,
			Timeout:           time.Minute,
		},
		Projects: map[string]config.ProjectConfig{
			"alpha": {
				Slug:         "alpha",
				RepoPath:     filepath.Join(dir, "repo"),
				WorktreeBase: filepath.Join(dir, "worktrees"),
				LedgerPath:   filepath.Join(dir, "repo", "tasks", "ledger.md"),
				Orchestrator: config.OrchestratorConfig{
					HeartbeatInterval: time.Minute,
					Timeout:           time.Minute,
				},
			},
		},
	}
}
