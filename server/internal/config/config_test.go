package config

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

	err := validateHarness(cfg, "worker", false, "")
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

func TestValidateProjectSlug(t *testing.T) {
	valid := []string{
		"alpha",
		"Alpha_01",
		"project-2026",
		"p",
	}
	for _, slug := range valid {
		t.Run("valid "+slug, func(t *testing.T) {
			if err := ValidateProjectSlug(slug); err != nil {
				t.Fatalf("ValidateProjectSlug(%q) error = %v, want nil", slug, err)
			}
		})
	}

	invalid := []string{
		"",
		"alpha/beta",
		"..",
		"alpha beta",
		"alpha\nbeta",
		"-alpha",
	}
	for _, slug := range invalid {
		t.Run("invalid "+strconv.Quote(slug), func(t *testing.T) {
			if err := ValidateProjectSlug(slug); err == nil {
				t.Fatalf("ValidateProjectSlug(%q) error = nil, want invalid slug", slug)
			}
		})
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

func TestPrepareRejectsBareBracketListenAddressWithoutAuth(t *testing.T) {
	cfg := &Config{
		Server:  ServerConfig{Host: "[]"},
		Runtime: RuntimeConfig{TmuxSession: "ccx-t2-test"},
	}

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want invalid listen host error")
	}
	if !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("Prepare() error = %v, want non-loopback guidance", err)
	}
}

func TestPrepareDefaultsEmptyListenAddressToLoopback(t *testing.T) {
	cfg := &Config{
		Runtime: RuntimeConfig{TmuxSession: "ccx-t2-test"},
	}

	if err := Prepare(cfg); err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("server.host = %q, want 127.0.0.1", cfg.Server.Host)
	}
}

func TestIsLoopbackListenHostTreatsEmptyAsUnsafe(t *testing.T) {
	if isLoopbackListenHost("") {
		t.Fatal("isLoopbackListenHost(\"\") = true, want false")
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

func TestPrepareAllowsValidProjectPaths(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	worktreeBase := mkdirConfigTestDir(t, "worktrees")
	cfg := &Config{
		Project: ProjectConfig{
			Slug:         "alpha",
			RepoPath:     repoPath,
			WorktreeBase: worktreeBase,
		},
	}

	if err := Prepare(cfg); err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	canonicalRepo := canonicalConfigTestPath(t, repoPath)
	canonicalWorktreeBase := canonicalConfigTestPath(t, worktreeBase)
	if cfg.Project.RepoPath != canonicalRepo {
		t.Fatalf("repo_path = %q, want %q", cfg.Project.RepoPath, canonicalRepo)
	}
	if cfg.Project.WorktreeBase != canonicalWorktreeBase {
		t.Fatalf("worktree_base = %q, want %q", cfg.Project.WorktreeBase, canonicalWorktreeBase)
	}
	if want := filepath.Join(canonicalRepo, "tasks", "ledger.md"); cfg.Project.LedgerPath != want {
		t.Fatalf("ledger_path = %q, want %q", cfg.Project.LedgerPath, want)
	}
}

func TestPrepareRejectsInvalidProjectSlugs(t *testing.T) {
	for _, slug := range []string{"alpha/beta", "..", "alpha beta", "alpha\nbeta"} {
		t.Run(strconv.Quote(slug), func(t *testing.T) {
			repoPath := initConfigTestRepo(t)
			cfg := configWithProjectPaths(t, slug, repoPath, mkdirConfigTestDir(t, "worktrees"), filepath.Join(repoPath, "tasks", "ledger.md"))

			err := Prepare(cfg)
			if err == nil {
				t.Fatal("Prepare() error = nil, want invalid project slug")
			}
			if !strings.Contains(err.Error(), "slug") || !strings.Contains(err.Error(), "must match") {
				t.Fatalf("Prepare() error = %v, want project slug guidance", err)
			}
		})
	}
}

func TestPrepareRejectsInvalidProjectMapKey(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	cfg := &Config{
		Runtime: RuntimeConfig{WorktreeBase: mkdirConfigTestDir(t, "worktrees")},
		Projects: map[string]ProjectConfig{
			"alpha/beta": {
				RepoPath:     repoPath,
				WorktreeBase: mkdirConfigTestDir(t, "project-worktrees"),
				LedgerPath:   filepath.Join(repoPath, "tasks", "ledger.md"),
			},
		},
	}

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want invalid project map key")
	}
	if !strings.Contains(err.Error(), "projects slug key") || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("Prepare() error = %v, want project map key guidance", err)
	}
}

func TestPrepareRejectsProjectSlugKeyMismatch(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	cfg := &Config{
		Runtime: RuntimeConfig{WorktreeBase: mkdirConfigTestDir(t, "worktrees")},
		Projects: map[string]ProjectConfig{
			"alpha": {
				Slug:         "beta",
				RepoPath:     repoPath,
				WorktreeBase: mkdirConfigTestDir(t, "project-worktrees"),
				LedgerPath:   filepath.Join(repoPath, "tasks", "ledger.md"),
			},
		},
	}

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want project slug key mismatch")
	}
	if !strings.Contains(err.Error(), "must match project key") {
		t.Fatalf("Prepare() error = %v, want project slug key mismatch guidance", err)
	}
}

func TestPrepareRejectsNonGitRepoPath(t *testing.T) {
	repoPath := mkdirConfigTestDir(t, "not-git")
	cfg := configWithProjectPaths(t, "alpha", repoPath, mkdirConfigTestDir(t, "worktrees"), filepath.Join(repoPath, "tasks", "ledger.md"))

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want invalid repo_path")
	}
	if !strings.Contains(err.Error(), "repo_path") || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("Prepare() error = %v, want git repo_path guidance", err)
	}
}

func TestPrepareRejectsLedgerPathOutsideRepo(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	ledgerPath := filepath.Join(t.TempDir(), "ledger.md")
	cfg := configWithProjectPaths(t, "alpha", repoPath, mkdirConfigTestDir(t, "worktrees"), ledgerPath)

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want unsafe ledger_path")
	}
	if !strings.Contains(err.Error(), "ledger_path") || !strings.Contains(err.Error(), "under repo_path") {
		t.Fatalf("Prepare() error = %v, want ledger containment guidance", err)
	}
}

func TestPrepareRejectsRepoInternalLedgerPathOutsideDefault(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	cfg := configWithProjectPaths(t, "alpha", repoPath, mkdirConfigTestDir(t, "worktrees"), filepath.Join(repoPath, ".git", "config"))

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want repo-internal arbitrary ledger_path rejection")
	}
	if !strings.Contains(err.Error(), "ledger_path") || !strings.Contains(err.Error(), "default repo ledger") {
		t.Fatalf("Prepare() error = %v, want default ledger guidance", err)
	}
}

func TestPrepareRejectsLedgerPathSymlinkToRepoInternalFile(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	tasksDir := filepath.Join(repoPath, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll tasks: %v", err)
	}
	ledgerPath := filepath.Join(tasksDir, "ledger.md")
	if err := os.Symlink(filepath.Join(repoPath, ".git", "config"), ledgerPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := configWithProjectPaths(t, "alpha", repoPath, mkdirConfigTestDir(t, "worktrees"), ledgerPath)

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want ledger_path symlink rejection")
	}
	if !strings.Contains(err.Error(), "ledger_path") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Prepare() error = %v, want symlink ledger guidance", err)
	}
}

func TestPrepareRejectsLedgerParentSymlinkToRepoInternalDirectory(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	targetDir := filepath.Join(repoPath, "internal-ledgers")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("MkdirAll target: %v", err)
	}
	linkPath := filepath.Join(repoPath, "tasks")
	if err := os.Symlink(targetDir, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := configWithProjectPaths(t, "alpha", repoPath, mkdirConfigTestDir(t, "worktrees"), filepath.Join(linkPath, "ledger.md"))

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want ledger parent symlink rejection")
	}
	if !strings.Contains(err.Error(), "ledger_path") || !strings.Contains(err.Error(), "default repo ledger") {
		t.Fatalf("Prepare() error = %v, want default ledger guidance", err)
	}
}

func TestPrepareRejectsLedgerPathSymlinkTraversal(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	outside := mkdirConfigTestDir(t, "outside")
	linkPath := filepath.Join(repoPath, "tasks")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := configWithProjectPaths(t, "alpha", repoPath, mkdirConfigTestDir(t, "worktrees"), filepath.Join(linkPath, "ledger.md"))

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want symlink ledger_path traversal rejection")
	}
	if !strings.Contains(err.Error(), "ledger_path") || !strings.Contains(err.Error(), "under repo_path") {
		t.Fatalf("Prepare() error = %v, want ledger containment guidance", err)
	}
}

func TestPrepareRejectsRelativeWorktreeBase(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	cfg := configWithProjectPaths(t, "alpha", repoPath, "relative-worktrees", filepath.Join(repoPath, "tasks", "ledger.md"))

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want relative worktree_base rejection")
	}
	if !strings.Contains(err.Error(), "worktree_base") || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Prepare() error = %v, want absolute worktree_base guidance", err)
	}
}

func TestProjectWorktreePathRejectsGeneratedTraversal(t *testing.T) {
	project := ProjectConfig{
		Slug:         "alpha",
		WorktreeBase: mkdirConfigTestDir(t, "worktrees"),
	}

	_, err := ProjectWorktreePath(project, "task-000/../../escape")
	if err == nil {
		t.Fatal("ProjectWorktreePath() error = nil, want generated worktree containment rejection")
	}
	if !strings.Contains(err.Error(), "generated worktree path") || !strings.Contains(err.Error(), "worktree_base") {
		t.Fatalf("ProjectWorktreePath() error = %v, want generated worktree containment guidance", err)
	}
}

func TestPrepareRejectsUnsafeTopLevelProjectPathOverrideWithProjects(t *testing.T) {
	projectRepo := initConfigTestRepo(t)
	projectWorktreeBase := mkdirConfigTestDir(t, "project-worktrees")
	topLevelRepo := initConfigTestRepo(t)
	topLevelWorktreeBase := mkdirConfigTestDir(t, "top-level-worktrees")
	cfg := &Config{
		Runtime: RuntimeConfig{WorktreeBase: projectWorktreeBase},
		Project: ProjectConfig{
			Slug:         "rogue",
			RepoPath:     topLevelRepo,
			WorktreeBase: topLevelWorktreeBase,
			LedgerPath:   filepath.Join(t.TempDir(), "ledger.md"),
		},
		Projects: map[string]ProjectConfig{
			"alpha": {
				Slug:         "alpha",
				RepoPath:     projectRepo,
				WorktreeBase: projectWorktreeBase,
				LedgerPath:   filepath.Join(projectRepo, "tasks", "ledger.md"),
			},
		},
	}

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want top-level project ledger_path rejection")
	}
	if !strings.Contains(err.Error(), "project.ledger_path") {
		t.Fatalf("Prepare() error = %v, want top-level project ledger_path guidance", err)
	}
}

func TestPrepareRejectsRelativeRuntimeWorktreeBase(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	projectWorktreeBase := mkdirConfigTestDir(t, "project-worktrees")
	cfg := &Config{
		Runtime: RuntimeConfig{WorktreeBase: "relative-worktrees"},
		Projects: map[string]ProjectConfig{
			"alpha": {
				Slug:         "alpha",
				RepoPath:     repoPath,
				WorktreeBase: projectWorktreeBase,
				LedgerPath:   filepath.Join(repoPath, "tasks", "ledger.md"),
			},
		},
	}

	err := Prepare(cfg)
	if err == nil {
		t.Fatal("Prepare() error = nil, want runtime.worktree_base rejection")
	}
	if !strings.Contains(err.Error(), "runtime.worktree_base") || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Prepare() error = %v, want runtime worktree_base guidance", err)
	}
}

func TestLoadGlobalProjectsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	worktreeBase := mkdirConfigTestDir(t, "ccx-worktrees")
	alphaRepo := initConfigTestRepo(t)
	betaRepo := initConfigTestRepo(t)
	betaLedger := filepath.Join(betaRepo, "tasks", "ledger.md")
	yaml := fmt.Sprintf(`
server:
  host: 0.0.0.0
  port: 18080
  allow_unsafe_no_auth: true
runtime:
  tmux_session: ccx-t2-test
  worktree_base: %s
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
    repo_path: %s
    validation_command: go test ./...
  beta:
    repo_path: %s
    ledger_path: %s
    orchestrator:
      harness: sh
`, worktreeBase, alphaRepo, betaRepo, betaLedger)
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
	if want := canonicalConfigTestPath(t, worktreeBase); alpha.WorktreeBase != want {
		t.Fatalf("alpha WorktreeBase = %q, want %q", alpha.WorktreeBase, want)
	}
	if want := filepath.Join(canonicalConfigTestPath(t, alphaRepo), "tasks", "ledger.md"); alpha.LedgerPath != want {
		t.Fatalf("alpha LedgerPath = %q, want %q", alpha.LedgerPath, want)
	}
	if alpha.Orchestrator.Harness != "sh" || alpha.Orchestrator.Timeout != cfg.Orchestrator.Timeout {
		t.Fatalf("alpha orchestrator = %#v", alpha.Orchestrator)
	}
	if got, want := cfg.Projects["beta"].LedgerPath, filepath.Join(canonicalConfigTestPath(t, betaRepo), "tasks", "ledger.md"); got != want {
		t.Fatalf("beta LedgerPath = %q, want %q", got, want)
	}
}

func TestLoadRejectsInvalidProjectSlug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	repoPath := initConfigTestRepo(t)
	worktreeBase := mkdirConfigTestDir(t, "worktrees")
	yaml := fmt.Sprintf(`
server:
  allow_unsafe_no_auth: true
runtime:
  tmux_session: ccx-t2-test
  worktree_base: %s
project:
  slug: alpha/beta
  repo_path: %s
`, worktreeBase, repoPath)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want invalid project slug")
	}
	if !strings.Contains(err.Error(), "slug") || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("Load() error = %v, want project slug guidance", err)
	}
}

func TestSaveRejectsInvalidProjectSlug(t *testing.T) {
	repoPath := initConfigTestRepo(t)
	cfg := configWithProjectPaths(t, "alpha beta", repoPath, mkdirConfigTestDir(t, "worktrees"), filepath.Join(repoPath, "tasks", "ledger.md"))

	err := Save(filepath.Join(t.TempDir(), "config.yaml"), cfg)
	if err == nil {
		t.Fatal("Save() error = nil, want invalid project slug")
	}
	if !strings.Contains(err.Error(), "slug") || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("Save() error = %v, want project slug guidance", err)
	}
}

func configWithProjectPaths(t *testing.T, slug, repoPath, worktreeBase, ledgerPath string) *Config {
	t.Helper()
	return &Config{
		Project: ProjectConfig{
			Slug:         slug,
			RepoPath:     repoPath,
			WorktreeBase: worktreeBase,
			LedgerPath:   ledgerPath,
		},
	}
}

func initConfigTestRepo(t *testing.T) string {
	t.Helper()
	repoPath := mkdirConfigTestDir(t, "repo")
	runConfigGit(t, repoPath, "init", "-q", "-b", "main")
	return repoPath
}

func mkdirConfigTestDir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
	return path
}

func runConfigGit(t *testing.T, repoPath string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", repoPath}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func canonicalConfigTestPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs %s: %v", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatalf("EvalSymlinks %s: %v", path, err)
	}
	return resolved
}
