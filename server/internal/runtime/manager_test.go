package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xpadev/ccx-t2/internal/config"
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

func TestRuntimeConfigClonesUseRoleSpecificMCPSecrets(t *testing.T) {
	cfg := testManagerConfig(t, "orchestrator")

	orchestratorCfg := orchestratorRuntimeConfig(cfg)
	if got := orchestratorCfg.Server.McpSecret; got != "orchestrator" {
		t.Fatalf("orchestrator runtime mcp_secret = %q, want orchestrator", got)
	}

	workerCfg := workerRuntimeConfig(cfg)
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
