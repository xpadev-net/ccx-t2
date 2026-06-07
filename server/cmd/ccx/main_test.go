package main

import (
	"os"
	"path/filepath"
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
	if _, ok := cfg.Projects[sanitizeSlug(filepath.Base(dir))]; !ok {
		t.Fatalf("generated projects = %#v, want cwd slug", cfg.Projects)
	}
}

func TestSanitizeSlug(t *testing.T) {
	for input, want := range map[string]string{
		"ccx-t2":       "ccx-t2",
		"My Project!!": "my-project",
		"___":          "project",
	} {
		if got := sanitizeSlug(input); got != want {
			t.Fatalf("sanitizeSlug(%q) = %q, want %q", input, got, want)
		}
	}
}
