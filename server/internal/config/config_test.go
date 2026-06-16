package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHarnessRejectsInvalidMCPArgsShellSyntax(t *testing.T) {
	cfg := &Config{
		Harnesses: map[string]HarnessConfig{
			"worker": {
				Command: "codex",
				McpArgs: "--mcp-url '{url}",
			},
		},
	}

	err := validateHarness(cfg, "worker", false)
	if err == nil {
		t.Fatal("validateHarness() error = nil, want invalid mcp_args shell syntax error")
	}
	if !strings.Contains(err.Error(), "mcp_args has invalid shell syntax") {
		t.Fatalf("validateHarness() error = %v, want mcp_args shell syntax error", err)
	}
}

func TestExpandEnvWarnsWhenReferenceExpandsEmpty(t *testing.T) {
	t.Setenv("MISSING_SECRET_FOR_TEST", "")
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg := &Config{Server: ServerConfig{
		McpSecret:          "${MISSING_SECRET_FOR_TEST}",
		WebAdminSecret:     "${MISSING_SECRET_FOR_TEST}",
		OrchestratorSecret: "${MISSING_SECRET_FOR_TEST}",
		WorkerSecret:       "${MISSING_SECRET_FOR_TEST}",
	}}
	expandEnv(cfg)

	if cfg.Server.McpSecret != "" {
		t.Fatalf("McpSecret = %q, want empty", cfg.Server.McpSecret)
	}
	if cfg.Server.WebAdminSecret != "" || cfg.Server.OrchestratorSecret != "" || cfg.Server.WorkerSecret != "" {
		t.Fatalf("role secrets = %#v, want empty", cfg.Server)
	}
	if !strings.Contains(buf.String(), "MISSING_SECRET_FOR_TEST") {
		t.Fatalf("log output %q does not mention missing env var", buf.String())
	}
}

func TestServerSecretsFallBackToLegacyMCPSecret(t *testing.T) {
	server := ServerConfig{McpSecret: "legacy"}

	if got := server.EffectiveWebAdminSecret(); got != "legacy" {
		t.Fatalf("EffectiveWebAdminSecret() = %q, want legacy", got)
	}
	if got := server.EffectiveOrchestratorSecret(); got != "legacy" {
		t.Fatalf("EffectiveOrchestratorSecret() = %q, want legacy", got)
	}
	if got := server.EffectiveWorkerSecret(); got != "legacy" {
		t.Fatalf("EffectiveWorkerSecret() = %q, want legacy", got)
	}
}

func TestServerRoleSecretsOverrideLegacyMCPSecret(t *testing.T) {
	server := ServerConfig{
		McpSecret:          "legacy",
		WebAdminSecret:     "web",
		OrchestratorSecret: "orchestrator",
		WorkerSecret:       "worker",
	}

	if got := server.EffectiveWebAdminSecret(); got != "web" {
		t.Fatalf("EffectiveWebAdminSecret() = %q, want web", got)
	}
	if got := server.EffectiveOrchestratorSecret(); got != "orchestrator" {
		t.Fatalf("EffectiveOrchestratorSecret() = %q, want orchestrator", got)
	}
	if got := server.EffectiveWorkerSecret(); got != "worker" {
		t.Fatalf("EffectiveWorkerSecret() = %q, want worker", got)
	}
}

func TestPrepareRejectsNonLoopbackListenAddressWithoutAuth(t *testing.T) {
	cfg := &Config{
		Server:  ServerConfig{Host: "0.0.0.0"},
		Runtime: RuntimeConfig{TmuxSession: "ccx-t2-test"},
	}

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want unsafe no-auth listen error")
	}
	if !strings.Contains(err.Error(), "server.allow_unsafe_no_auth") {
		t.Fatalf("Prepare() error = %v, want allow_unsafe_no_auth guidance", err)
	}
}

func TestPrepareAllowsNonLoopbackListenAddressWithRoleSecrets(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Host:               "0.0.0.0",
			WebAdminSecret:     "web",
			OrchestratorSecret: "orchestrator",
			WorkerSecret:       "worker",
		},
		Runtime: RuntimeConfig{TmuxSession: "ccx-t2-test"},
	}

	if err := Prepare(cfg); err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
}

func TestPrepareAllowsNonLoopbackListenAddressWithLegacyMCPSecret(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Host:      "0.0.0.0",
			McpSecret: "legacy",
		},
		Runtime: RuntimeConfig{TmuxSession: "ccx-t2-test"},
	}

	if err := Prepare(cfg); err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
}

func TestPrepareAllowsHarnessSecretPlaceholderWithRoleSecrets(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			OrchestratorSecret: "orchestrator",
			WorkerSecret:       "worker",
		},
		Runtime: RuntimeConfig{TmuxSession: "ccx-t2-test"},
		Orchestrator: OrchestratorConfig{
			Harness: "orchestrator",
		},
		WorkerHarnesses: []string{"worker"},
		Harnesses: map[string]HarnessConfig{
			"orchestrator": {
				Command: "sh",
				McpArgs: "--mcp-url {url} --token {secret}",
			},
			"worker": {
				Command: "worker",
				McpArgs: "--mcp-url {url} --token {secret}",
			},
		},
	}

	if err := Prepare(cfg); err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
}

func TestPrepareRejectsNonLoopbackListenAddressWithPartialAuth(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Host:         "0.0.0.0",
			WorkerSecret: "worker",
		},
		Runtime: RuntimeConfig{TmuxSession: "ccx-t2-test"},
	}

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want partial auth listen error")
	}
	if !strings.Contains(err.Error(), "one or more Web/MCP auth secrets are empty") {
		t.Fatalf("Prepare() error = %v, want partial auth guidance", err)
	}
}

func TestPrepareAllowsExplicitUnsafeNoAuthNonLoopbackListenAddress(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Host:              "0.0.0.0",
			AllowUnsafeNoAuth: true,
		},
		Runtime: RuntimeConfig{TmuxSession: "ccx-t2-test"},
	}

	if err := Prepare(cfg); err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
}

func TestLoadGlobalProjectsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  host: 0.0.0.0
  port: 18080
  allow_unsafe_no_auth: true
runtime:
  tmux_session: ccx-t2-test
  worktree_base: /tmp/ccx-worktrees
orchestrator:
  harness: sh
  heartbeat_interval: 30s
  timeout: 2m
worker_harnesses: [sh]
harnesses:
  sh:
    command: sh
    mcp_args: "--mcp-url {url}"
projects:
  alpha:
    repo_path: /repo/alpha
    validation_command: go test ./...
  beta:
    repo_path: /repo/beta
    ledger_path: /custom/beta-ledger.md
    orchestrator:
      harness: sh
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("server.host = %q, want 0.0.0.0", cfg.Server.Host)
	}

	alpha := cfg.Projects["alpha"]
	if alpha.WorktreeBase != "/tmp/ccx-worktrees" {
		t.Fatalf("alpha WorktreeBase = %q", alpha.WorktreeBase)
	}
	if alpha.LedgerPath != "/repo/alpha/tasks/ledger.md" {
		t.Fatalf("alpha LedgerPath = %q", alpha.LedgerPath)
	}
	if alpha.Orchestrator.Harness != "sh" || alpha.Orchestrator.Timeout != cfg.Orchestrator.Timeout {
		t.Fatalf("alpha orchestrator = %#v", alpha.Orchestrator)
	}
	if got := cfg.Projects["beta"].LedgerPath; got != "/custom/beta-ledger.md" {
		t.Fatalf("beta LedgerPath = %q", got)
	}
}
