package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"gopkg.in/yaml.v3"
)

// HarnessConfig defines a single harness entry.
type HarnessConfig struct {
	Command string `yaml:"command"`
	McpArgs string `yaml:"mcp_args"`
}

// ProjectConfig holds project-level settings.
type ProjectConfig struct {
	Slug              string             `yaml:"slug"`
	RepoPath          string             `yaml:"repo_path"`
	WorktreeBase      string             `yaml:"worktree_base"`
	LedgerPath        string             `yaml:"ledger_path,omitempty"`
	ValidationCommand string             `yaml:"validation_command"`
	Orchestrator      OrchestratorConfig `yaml:"orchestrator,omitempty"`
	GitHub            GitHubConfig       `yaml:"github,omitempty"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host               string `yaml:"host"`
	Port               int    `yaml:"port"`
	McpSecret          string `yaml:"mcp_secret"` // legacy Bearer token; seeds role-specific secrets when they are unset
	WebAdminSecret     string `yaml:"web_admin_secret"`
	OrchestratorSecret string `yaml:"orchestrator_mcp_secret"`
	WorkerSecret       string `yaml:"worker_mcp_secret"`
	AllowUnsafeNoAuth  bool   `yaml:"allow_unsafe_no_auth"`
}

// RuntimeConfig holds process-wide runtime settings.
type RuntimeConfig struct {
	TmuxSession  string `yaml:"tmux_session"`
	WorktreeBase string `yaml:"worktree_base"`
}

// OrchestratorConfig holds Orchestrator settings.
type OrchestratorConfig struct {
	Harness           string        `yaml:"harness"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	Timeout           time.Duration `yaml:"timeout"`
}

// GitHubConfig holds GitHub API credentials.
type GitHubConfig struct {
	Token string `yaml:"token"`
	Owner string `yaml:"owner"`
	Repo  string `yaml:"repo"`
}

// Config is the top-level configuration structure.
type Config struct {
	Project         ProjectConfig            `yaml:"project"`
	Server          ServerConfig             `yaml:"server"`
	Runtime         RuntimeConfig            `yaml:"runtime"`
	Orchestrator    OrchestratorConfig       `yaml:"orchestrator"`
	WorkerHarnesses []string                 `yaml:"worker_harnesses"`
	Harnesses       map[string]HarnessConfig `yaml:"harnesses"`
	GitHub          GitHubConfig             `yaml:"github"`
	Projects        map[string]ProjectConfig `yaml:"projects"`
}

// Load reads and validates the config from the given file path.
func Load(path string) (*Config, error) {
	cfg, err := LoadRaw(path)
	if err != nil {
		return nil, err
	}
	if err := Prepare(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadRaw reads the config without expanding environment variables or applying
// defaults. It is intended for edit flows that must preserve placeholders.
func LoadRaw(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Save validates cfg through the normal load pipeline, then writes it atomically.
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config: nil config")
	}
	check := Clone(cfg)
	if err := Prepare(check); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Prepare applies runtime normalization and validation in place.
func Prepare(cfg *Config) error {
	expandEnv(cfg)
	applyDefaults(cfg)
	normalizeProjects(cfg)

	if err := validate(cfg); err != nil {
		return err
	}
	return nil
}

// Clone returns a deep enough copy for independent mutation of slices and maps.
func Clone(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.WorkerHarnesses = append([]string(nil), cfg.WorkerHarnesses...)
	out.Harnesses = make(map[string]HarnessConfig, len(cfg.Harnesses))
	for name, h := range cfg.Harnesses {
		out.Harnesses[name] = h
	}
	out.Projects = make(map[string]ProjectConfig, len(cfg.Projects))
	for slug, project := range cfg.Projects {
		out.Projects[slug] = project
	}
	return &out
}

// reEnvVar matches ${VAR} placeholders only (not bare $VAR).
// Restricting to the braced form prevents accidental expansion of shell
// special variables like $@, $1, $2 that may appear in validation_command.
var reEnvVar = regexp.MustCompile(`\$\{([^}]+)\}`)

// expandEnv replaces ${VAR} patterns with environment variable values.
// Bare $VAR patterns (e.g. $@, $1) are left untouched.
// Unknown variable names expand to an empty string (os.Getenv behaviour).
// Callers that configure optional security fields (e.g. mcp_secret) via env
// vars should ensure the referenced variable is actually set; an undefined
// variable will silently disable the associated protection.
func expandEnv(cfg *Config) {
	expand := func(s string) string {
		return reEnvVar.ReplaceAllStringFunc(s, func(match string) string {
			key := match[2 : len(match)-1] // strip ${ and }
			val := os.Getenv(key)
			if val == "" {
				log.Printf("warn: environment variable %s referenced in config expands to empty string", key)
			}
			return val
		})
	}

	cfg.Project.Slug = expand(cfg.Project.Slug)
	cfg.Project.RepoPath = expand(cfg.Project.RepoPath)
	cfg.Project.WorktreeBase = expand(cfg.Project.WorktreeBase)
	cfg.Project.LedgerPath = expand(cfg.Project.LedgerPath)
	cfg.Project.ValidationCommand = expand(cfg.Project.ValidationCommand)
	cfg.Project.Orchestrator.Harness = expand(cfg.Project.Orchestrator.Harness)
	cfg.Project.GitHub.Token = expand(cfg.Project.GitHub.Token)
	cfg.Project.GitHub.Owner = expand(cfg.Project.GitHub.Owner)
	cfg.Project.GitHub.Repo = expand(cfg.Project.GitHub.Repo)
	cfg.Server.Host = expand(cfg.Server.Host)
	cfg.Server.WebAdminSecret = expand(cfg.Server.WebAdminSecret)
	cfg.Server.OrchestratorSecret = expand(cfg.Server.OrchestratorSecret)
	cfg.Server.WorkerSecret = expand(cfg.Server.WorkerSecret)
	cfg.Runtime.TmuxSession = expand(cfg.Runtime.TmuxSession)
	cfg.Runtime.WorktreeBase = expand(cfg.Runtime.WorktreeBase)
	cfg.Orchestrator.Harness = expand(cfg.Orchestrator.Harness)
	cfg.GitHub.Token = expand(cfg.GitHub.Token)
	cfg.GitHub.Owner = expand(cfg.GitHub.Owner)
	cfg.GitHub.Repo = expand(cfg.GitHub.Repo)
	cfg.Server.McpSecret = expand(cfg.Server.McpSecret)

	for i, name := range cfg.WorkerHarnesses {
		cfg.WorkerHarnesses[i] = expand(name)
	}
	for name, h := range cfg.Harnesses {
		h.Command = expand(h.Command)
		h.McpArgs = expand(h.McpArgs)
		cfg.Harnesses[name] = h
	}
	for slug, project := range cfg.Projects {
		project.Slug = expand(project.Slug)
		project.RepoPath = expand(project.RepoPath)
		project.WorktreeBase = expand(project.WorktreeBase)
		project.LedgerPath = expand(project.LedgerPath)
		project.ValidationCommand = expand(project.ValidationCommand)
		project.Orchestrator.Harness = expand(project.Orchestrator.Harness)
		project.GitHub.Token = expand(project.GitHub.Token)
		project.GitHub.Owner = expand(project.GitHub.Owner)
		project.GitHub.Repo = expand(project.GitHub.Repo)
		cfg.Projects[slug] = project
	}
}

// applyDefaults sets default values for optional fields.
func applyDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "127.0.0.1"
	}
	if cfg.Runtime.TmuxSession == "" {
		cfg.Runtime.TmuxSession = "ccx-t2"
	}
	if cfg.Orchestrator.HeartbeatInterval == 0 {
		cfg.Orchestrator.HeartbeatInterval = 60 * time.Second
	}
	if cfg.Orchestrator.Timeout == 0 {
		cfg.Orchestrator.Timeout = 30 * time.Minute
	}
}

func normalizeProjects(cfg *Config) {
	if cfg.Projects == nil {
		cfg.Projects = make(map[string]ProjectConfig)
	}
	if len(cfg.Projects) == 0 && strings.TrimSpace(cfg.Project.Slug) != "" {
		project := cfg.Project
		if project.GitHub == (GitHubConfig{}) {
			project.GitHub = cfg.GitHub
		}
		if project.Orchestrator == (OrchestratorConfig{}) {
			project.Orchestrator = cfg.Orchestrator
		}
		cfg.Projects[project.Slug] = project
	}
	for slug, project := range cfg.Projects {
		if project.Slug == "" {
			project.Slug = slug
		}
		if project.WorktreeBase == "" {
			project.WorktreeBase = cfg.Runtime.WorktreeBase
		}
		if project.LedgerPath == "" && project.RepoPath != "" {
			project.LedgerPath = filepath.Join(project.RepoPath, "tasks", "ledger.md")
		}
		if project.Orchestrator.Harness == "" {
			project.Orchestrator.Harness = cfg.Orchestrator.Harness
		}
		if project.Orchestrator.HeartbeatInterval == 0 {
			project.Orchestrator.HeartbeatInterval = cfg.Orchestrator.HeartbeatInterval
		}
		if project.Orchestrator.Timeout == 0 {
			project.Orchestrator.Timeout = cfg.Orchestrator.Timeout
		}
		if project.GitHub == (GitHubConfig{}) {
			project.GitHub = cfg.GitHub
		}
		cfg.Projects[slug] = project
	}
	if cfg.Project.Slug == "" && len(cfg.Projects) > 0 {
		slugs := make([]string, 0, len(cfg.Projects))
		for slug := range cfg.Projects {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		project := cfg.Projects[slugs[0]]
		project.Slug = slugs[0]
		cfg.Project = project
		cfg.GitHub = project.GitHub
	}
}

// validate checks required fields and harness configuration.
func validate(cfg *Config) error {
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("config: server.port %d out of range [1, 65535]", cfg.Server.Port)
	}
	if strings.ContainsAny(cfg.Server.Host, " \t\r\n") {
		return fmt.Errorf("config: server.host must not contain whitespace")
	}
	if !cfg.Server.AllowUnsafeNoAuth && !isLoopbackListenHost(cfg.Server.Host) && !allServerSecretsConfigured(cfg.Server) {
		return fmt.Errorf("config: server.host %q is not loopback and one or more Web/MCP auth secrets are empty; set server.web_admin_secret, server.orchestrator_mcp_secret, server.worker_mcp_secret, or legacy server.mcp_secret, or explicitly set server.allow_unsafe_no_auth: true", cfg.Server.Host)
	}
	if strings.TrimSpace(cfg.Runtime.TmuxSession) == "" {
		return fmt.Errorf("config: required field %q is empty", "runtime.tmux_session")
	}
	if strings.TrimSpace(cfg.Runtime.WorktreeBase) == "" && strings.TrimSpace(cfg.Project.WorktreeBase) != "" {
		cfg.Runtime.WorktreeBase = cfg.Project.WorktreeBase
	}
	if cfg.Orchestrator.HeartbeatInterval <= 0 {
		return fmt.Errorf("config: orchestrator.heartbeat_interval must be positive")
	}
	if cfg.Orchestrator.Timeout <= 0 {
		return fmt.Errorf("config: orchestrator.timeout must be positive")
	}

	if cfg.Orchestrator.Harness != "" {
		if err := validateHarness(cfg, cfg.Orchestrator.Harness, true, cfg.Server.EffectiveOrchestratorSecret()); err != nil {
			return fmt.Errorf("config: orchestrator harness %q: %w", cfg.Orchestrator.Harness, err)
		}
	}

	// Validate worker harnesses (binary check deferred to spawn time).
	for _, name := range cfg.WorkerHarnesses {
		if err := validateHarness(cfg, name, false, cfg.Server.EffectiveWorkerSecret()); err != nil {
			return fmt.Errorf("config: worker harness %q: %w", name, err)
		}
	}
	for slug, project := range cfg.Projects {
		if strings.TrimSpace(slug) == "" {
			return fmt.Errorf("config: project slug cannot be empty")
		}
		for _, f := range []struct{ name, val string }{
			{"projects." + slug + ".repo_path", project.RepoPath},
			{"projects." + slug + ".worktree_base", project.WorktreeBase},
			{"projects." + slug + ".ledger_path", project.LedgerPath},
		} {
			if strings.TrimSpace(f.val) == "" {
				return fmt.Errorf("config: required field %q is empty", f.name)
			}
		}
		if project.Orchestrator.HeartbeatInterval <= 0 {
			return fmt.Errorf("config: projects.%s.orchestrator.heartbeat_interval must be positive", slug)
		}
		if project.Orchestrator.Timeout <= 0 {
			return fmt.Errorf("config: projects.%s.orchestrator.timeout must be positive", slug)
		}
		if project.Orchestrator.Harness != "" {
			if err := validateHarness(cfg, project.Orchestrator.Harness, true, cfg.Server.EffectiveOrchestratorSecret()); err != nil {
				return fmt.Errorf("config: project %q orchestrator harness %q: %w", slug, project.Orchestrator.Harness, err)
			}
		}
	}

	return nil
}

// EffectiveWebAdminSecret returns the browser/admin API secret, falling back to
// the legacy shared MCP secret for existing configs.
func (s ServerConfig) EffectiveWebAdminSecret() string {
	if s.WebAdminSecret != "" {
		return s.WebAdminSecret
	}
	return s.McpSecret
}

// EffectiveOrchestratorSecret returns the orchestrator MCP secret, falling back
// to the legacy shared MCP secret for existing configs.
func (s ServerConfig) EffectiveOrchestratorSecret() string {
	if s.OrchestratorSecret != "" {
		return s.OrchestratorSecret
	}
	return s.McpSecret
}

// EffectiveWorkerSecret returns the worker MCP secret, falling back to the
// legacy shared MCP secret for existing configs.
func (s ServerConfig) EffectiveWorkerSecret() string {
	if s.WorkerSecret != "" {
		return s.WorkerSecret
	}
	return s.McpSecret
}

func allServerSecretsConfigured(s ServerConfig) bool {
	return s.EffectiveWebAdminSecret() != "" &&
		s.EffectiveOrchestratorSecret() != "" &&
		s.EffectiveWorkerSecret() != ""
}

func isLoopbackListenHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return false
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if host == "" {
			return false
		}
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CloneForOrchestratorRuntime returns cfg with the legacy MCP secret slot set
// to the effective orchestrator MCP secret for existing harness code paths.
func CloneForOrchestratorRuntime(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	out := Clone(cfg)
	out.Server.McpSecret = cfg.Server.EffectiveOrchestratorSecret()
	return out
}

// CloneForWorkerRuntime returns cfg with the legacy MCP secret slot set to the
// effective worker MCP secret for existing harness code paths.
func CloneForWorkerRuntime(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	out := Clone(cfg)
	out.Server.McpSecret = cfg.Server.EffectiveWorkerSecret()
	return out
}

// Project returns a project-scoped copy of cfg. Shared harness/server/runtime
// settings are preserved while Project, Orchestrator, and GitHub are resolved
// for the requested project.
func Project(cfg *Config, slug string) (*Config, bool) {
	if cfg == nil {
		return nil, false
	}
	project, ok := cfg.Projects[slug]
	if !ok {
		return nil, false
	}
	out := Clone(cfg)
	out.Project = project
	out.Orchestrator = project.Orchestrator
	out.GitHub = project.GitHub
	return out, true
}

func validateHarness(cfg *Config, name string, checkBinary bool, secret string) error {
	h, ok := cfg.Harnesses[name]
	if !ok {
		return fmt.Errorf("not found in harnesses")
	}
	if strings.TrimSpace(h.Command) == "" {
		return fmt.Errorf("command is empty")
	}
	if strings.ContainsAny(h.Command, " \t\n") {
		return fmt.Errorf("command must be a single binary name or path with no whitespace; got %q", h.Command)
	}
	if strings.TrimSpace(h.McpArgs) == "" {
		return fmt.Errorf("mcp_args is empty")
	}
	if !strings.Contains(h.McpArgs, "{url}") {
		return fmt.Errorf("mcp_args must contain the {url} placeholder")
	}
	if _, err := shellquote.Split(h.McpArgs); err != nil {
		return fmt.Errorf("mcp_args has invalid shell syntax: %w", err)
	}
	if secret != "" && !strings.Contains(h.McpArgs, "{secret}") {
		return fmt.Errorf("MCP secret is configured but mcp_args does not contain {secret}; the harness will receive 401 on every MCP call")
	}
	if secret == "" && strings.Contains(h.McpArgs, "{secret}") {
		return fmt.Errorf("mcp_args contains {secret} placeholder but MCP secret is not configured; {secret} will expand to empty string")
	}
	if checkBinary {
		if _, err := exec.LookPath(h.Command); err != nil {
			return fmt.Errorf("binary %q not found in PATH: %w", h.Command, err)
		}
	}
	return nil
}
