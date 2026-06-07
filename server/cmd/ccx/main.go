package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/mcp"
	runtimepkg "github.com/xpadev/ccx-t2/internal/runtime"
	"github.com/xpadev/ccx-t2/internal/tmux"
	"github.com/xpadev/ccx-t2/internal/web"
	"github.com/xpadev/ccx-t2/internal/webui"
	"gopkg.in/yaml.v3"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: ccx [serve] [--config path] [--web-dir path]")
		os.Exit(2)
	}
	if err := serve(args); err != nil {
		log.Fatal(err)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "global config path")
	webDir := fs.String("web-dir", "web/dist", "built Web UI directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := ensureConfig(*configPath); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := tmux.EnsureSession(cfg.Runtime.TmuxSession); err != nil {
		return fmt.Errorf("ensure tmux session: %w", err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port)
	manager, err := runtimepkg.NewManager(cfg, baseURL)
	if err != nil {
		return err
	}

	orchestratorMCP := mcp.NewServer("orchestrator", cfg.Server.McpSecret)
	workerMCP := mcp.NewServer("worker", cfg.Server.McpSecret)
	mcpDeps := &mcp.Deps{
		Config:  cfg,
		Session: cfg.Runtime.TmuxSession,
		BaseURL: baseURL,
		Manager: manager,
	}
	mcp.RegisterOrchestratorTools(orchestratorMCP, mcpDeps)
	mcp.RegisterWorkerTools(workerMCP, mcpDeps)

	api := web.New(web.Deps{
		Config:       cfg,
		ConfigPath:   *configPath,
		Manager:      manager,
		Session:      cfg.Runtime.TmuxSession,
		Secret:       cfg.Server.McpSecret,
		AuthDisabled: cfg.Server.McpSecret == "",
	})

	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.Handle("/ws/", api)
	mux.Handle("/mcp/orchestrator", orchestratorMCP)
	mux.Handle("/mcp/worker", workerMCP)
	if info, err := os.Stat(*webDir); err == nil && info.IsDir() {
		mux.Handle("/", http.FileServer(http.Dir(*webDir)))
	} else {
		handler, err := webui.Handler()
		if err != nil {
			return fmt.Errorf("embedded web ui: %w", err)
		}
		mux.Handle("/", handler)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manager.Start(ctx)
	defer manager.Close()

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("ccx serving on %s", baseURL)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func defaultConfigPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "ccx-t2", "config.yaml")
	}
	return "config.yaml"
}

func ensureConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config: %w", err)
	}

	cfg, err := defaultConfig()
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal default config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	log.Printf("created default config at %s", path)
	return nil
}

func defaultConfig() (*config.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get cwd: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = cwd
	}
	slug := sanitizeSlug(filepath.Base(cwd))
	worktreeBase := filepath.Join(home, ".local", "share", "ccx-t2", "worktrees")
	project := config.ProjectConfig{
		Slug:              slug,
		RepoPath:          cwd,
		WorktreeBase:      worktreeBase,
		LedgerPath:        filepath.Join(cwd, "tasks", "ledger.md"),
		ValidationCommand: "go test ./...",
		Orchestrator: config.OrchestratorConfig{
			Harness:           "sh",
			HeartbeatInterval: time.Minute,
			Timeout:           30 * time.Minute,
		},
	}
	return &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Runtime: config.RuntimeConfig{
			TmuxSession:  "ccx-t2",
			WorktreeBase: worktreeBase,
		},
		Orchestrator: config.OrchestratorConfig{
			Harness:           "sh",
			HeartbeatInterval: time.Minute,
			Timeout:           30 * time.Minute,
		},
		WorkerHarnesses: []string{"sh"},
		Harnesses: map[string]config.HarnessConfig{
			"sh": {
				Command: "sh",
				McpArgs: "--mcp-url {url}",
			},
		},
		Projects: map[string]config.ProjectConfig{slug: project},
	}, nil
}

var reSlugChar = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeSlug(s string) string {
	s = strings.Trim(reSlugChar.ReplaceAllString(s, "-"), "-_")
	if s == "" {
		return "project"
	}
	return strings.ToLower(s)
}
