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
