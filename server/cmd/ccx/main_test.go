package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xpadev/ccx-t2/internal/config"
)

func TestEnsureConfigCreatesDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir bin: %v", err)
	}
	fakeCodex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\necho 'usage: codex --yolo --mcp-url'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile fake codex: %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Cleanup(func() {
		if err := os.Chdir(prevWD); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	path := filepath.Join(dir, "config.yaml")
	if err := ensureConfig(path); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load generated config: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Fatalf("server.port = %d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("server.host = %q, want 127.0.0.1", cfg.Server.Host)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("generated projects = %#v, want empty", cfg.Projects)
	}
	if len(cfg.WorkerHarnesses) != 1 || cfg.WorkerHarnesses[0] != "codex" {
		t.Fatalf("worker_harnesses = %#v, want codex only", cfg.WorkerHarnesses)
	}
	if got := cfg.Harnesses["codex"].McpArgs; !strings.Contains(got, "--yolo") || !strings.Contains(got, "{url}") {
		t.Fatalf("codex mcp_args = %q, want yolo and mcp url", got)
	}
}

func TestBaseURLHostForWildcardListenAddress(t *testing.T) {
	for input, want := range map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "127.0.0.1",
		"127.0.0.1": "127.0.0.1",
		"localhost": "localhost",
	} {
		if got := baseURLHost(input); got != want {
			t.Fatalf("baseURLHost(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestServeRejectsPublicNoAuthConfigAtStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte(`
server:
  host: 0.0.0.0
  port: 18080
runtime:
  tmux_session: ccx-t2-test
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := serve([]string{"--config", path})
	if err == nil {
		t.Fatal("serve() error = nil, want unsafe no-auth config error")
	}
	if !strings.Contains(err.Error(), "allow_unsafe_no_auth") {
		t.Fatalf("serve() error = %v, want allow_unsafe_no_auth guidance", err)
	}
}

func TestCloneForWorkerRuntimeUsesWorkerMCPSecret(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{
		McpSecret:    "legacy",
		WorkerSecret: "worker",
	}}

	workerCfg := config.CloneForWorkerRuntime(cfg)
	if got := workerCfg.Server.McpSecret; got != "worker" {
		t.Fatalf("worker runtime mcp_secret = %q, want worker", got)
	}

	if got := cfg.Server.McpSecret; got != "legacy" {
		t.Fatalf("source mcp_secret = %q, want legacy", got)
	}
}

func TestDefaultConfigAllowsNoDetectedHarnesses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("projects = %#v, want empty", cfg.Projects)
	}
	if len(cfg.WorkerHarnesses) != 0 || len(cfg.Harnesses) != 0 {
		t.Fatalf("harnesses = %#v/%#v, want none", cfg.WorkerHarnesses, cfg.Harnesses)
	}
	if err := config.Prepare(cfg); err != nil {
		t.Fatalf("Prepare default config without harnesses: %v", err)
	}
}
