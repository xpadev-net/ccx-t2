package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

	listenAddr := net.JoinHostPort(cfg.Server.Host, fmt.Sprint(cfg.Server.Port))
	baseURL := fmt.Sprintf("http://%s", net.JoinHostPort(baseURLHost(cfg.Server.Host), fmt.Sprint(cfg.Server.Port)))
	manager, err := runtimepkg.NewManager(cfg, baseURL)
	if err != nil {
		return err
	}

	webAdminSecret := cfg.Server.EffectiveWebAdminSecret()
	orchestratorMCP := mcp.NewServer("orchestrator", cfg.Server.EffectiveOrchestratorSecret())
	workerMCP := mcp.NewServer("worker", cfg.Server.EffectiveWorkerSecret())
	mcpDeps := &mcp.Deps{
		Config:  config.CloneForWorkerRuntime(cfg),
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
		Secret:       webAdminSecret,
		AuthDisabled: webAdminSecret == "",
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

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Printf("ccx serving on %s (listen %s)", baseURL, listenAddr)
		errCh <- server.Serve(listener)
	}()
	manager.Start(ctx)
	defer manager.Close()

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

func baseURLHost(host string) string {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return strings.Trim(host, "[]")
	}
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
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	worktreeBase := filepath.Join(home, ".local", "share", "ccx-t2", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
		return nil, fmt.Errorf("create default worktree base: %w", err)
	}
	workerHarnesses, harnesses := detectHarnesses()
	orchestratorHarness := ""
	if len(workerHarnesses) > 0 {
		orchestratorHarness = workerHarnesses[0]
	}
	return &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 8080},
		Runtime: config.RuntimeConfig{
			TmuxSession:  "ccx-t2",
			WorktreeBase: worktreeBase,
		},
		Orchestrator: config.OrchestratorConfig{
			Harness:           orchestratorHarness,
			HeartbeatInterval: time.Minute,
			Timeout:           30 * time.Minute,
		},
		WorkerHarnesses: workerHarnesses,
		Harnesses:       harnesses,
		Projects:        map[string]config.ProjectConfig{},
	}, nil
}

type harnessCandidate struct {
	name         string
	fallbackArgs []string
}

var harnessCandidates = []harnessCandidate{
	{name: "claude", fallbackArgs: []string{"--dangerously-skip-permissions"}},
	{name: "codex", fallbackArgs: []string{"--yolo"}},
	{name: "opencode", fallbackArgs: []string{"--permission-mode", "yolo"}},
	{name: "cursor-agent", fallbackArgs: []string{"--yolo"}},
}

func detectHarnesses() ([]string, map[string]config.HarnessConfig) {
	names := make([]string, 0, len(harnessCandidates))
	harnesses := make(map[string]config.HarnessConfig)
	for _, candidate := range harnessCandidates {
		if _, err := exec.LookPath(candidate.name); err != nil {
			continue
		}
		args := append(dangerousPermissionArgs(candidate), "--mcp-url", "{url}")
		names = append(names, candidate.name)
		harnesses[candidate.name] = config.HarnessConfig{
			Command: candidate.name,
			McpArgs: strings.Join(args, " "),
		}
	}
	return names, harnesses
}

func dangerousPermissionArgs(candidate harnessCandidate) []string {
	help := commandHelp(candidate.name)
	switch {
	case strings.Contains(help, "--dangerously-skip-permissions"):
		return []string{"--dangerously-skip-permissions"}
	case strings.Contains(help, "--dangerously-bypass-approvals-and-sandbox"):
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	case strings.Contains(help, "--allow-dangerous-permissions"):
		return []string{"--allow-dangerous-permissions"}
	case strings.Contains(help, "--yolo"):
		return []string{"--yolo"}
	case strings.Contains(help, "--permission-mode") && strings.Contains(strings.ToLower(help), "yolo"):
		return []string{"--permission-mode", "yolo"}
	}
	return candidate.fallbackArgs
}

func commandHelp(command string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, command, "--help").CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return string(out)
}
