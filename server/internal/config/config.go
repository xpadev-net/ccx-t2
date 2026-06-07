package config

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	Slug              string `yaml:"slug"`
	RepoPath          string `yaml:"repo_path"`
	WorktreeBase      string `yaml:"worktree_base"`
	ValidationCommand string `yaml:"validation_command"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port      int    `yaml:"port"`
	McpSecret string `yaml:"mcp_secret"` // optional Bearer token for MCP endpoints
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
	Orchestrator    OrchestratorConfig       `yaml:"orchestrator"`
	WorkerHarnesses []string                 `yaml:"worker_harnesses"`
	Harnesses       map[string]HarnessConfig `yaml:"harnesses"`
	GitHub          GitHubConfig             `yaml:"github"`
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
	cfg.Project.ValidationCommand = expand(cfg.Project.ValidationCommand)
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
}

// applyDefaults sets default values for optional fields.
func applyDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Orchestrator.HeartbeatInterval == 0 {
		cfg.Orchestrator.HeartbeatInterval = 60 * time.Second
	}
	if cfg.Orchestrator.Timeout == 0 {
		cfg.Orchestrator.Timeout = 30 * time.Minute
	}
}

// validate checks required fields and harness configuration.
func validate(cfg *Config) error {
	// Check required string fields in a deterministic order.
	type requiredField struct{ name, val string }
	for _, f := range []requiredField{
		{"project.slug", cfg.Project.Slug},
		{"project.repo_path", cfg.Project.RepoPath},
		{"project.worktree_base", cfg.Project.WorktreeBase},
		{"orchestrator.harness", cfg.Orchestrator.Harness},
	} {
		if strings.TrimSpace(f.val) == "" {
			return fmt.Errorf("config: required field %q is empty", f.name)
		}
	}

	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("config: server.port %d out of range [1, 65535]", cfg.Server.Port)
	}
	if cfg.Orchestrator.HeartbeatInterval <= 0 {
		return fmt.Errorf("config: orchestrator.heartbeat_interval must be positive")
	}
	if cfg.Orchestrator.Timeout <= 0 {
		return fmt.Errorf("config: orchestrator.timeout must be positive")
	}

	if len(cfg.WorkerHarnesses) == 0 {
		return fmt.Errorf("config: worker_harnesses must have at least one entry")
	}

	// Validate orchestrator harness.
	if err := validateHarness(cfg, cfg.Orchestrator.Harness, true); err != nil {
		return fmt.Errorf("config: orchestrator harness %q: %w", cfg.Orchestrator.Harness, err)
	}

	// Validate worker harnesses (binary check deferred to spawn time).
	for _, name := range cfg.WorkerHarnesses {
		if err := validateHarness(cfg, name, false); err != nil {
			return fmt.Errorf("config: worker harness %q: %w", name, err)
		}
	}

	return nil
}

func validateHarness(cfg *Config, name string, checkBinary bool) error {
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
	if cfg.Server.McpSecret != "" && !strings.Contains(h.McpArgs, "{secret}") {
		return fmt.Errorf("mcp_secret is configured but mcp_args does not contain {secret}; the harness will receive 401 on every MCP call")
	}
	if cfg.Server.McpSecret == "" && strings.Contains(h.McpArgs, "{secret}") {
		return fmt.Errorf("mcp_args contains {secret} placeholder but mcp_secret is not configured; {secret} will expand to empty string")
	}
	if checkBinary {
		if _, err := exec.LookPath(h.Command); err != nil {
			return fmt.Errorf("binary %q not found in PATH: %w", h.Command, err)
		}
	}
	return nil
}
