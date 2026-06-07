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
	"syscall"
	"time"

	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/mcp"
	runtimepkg "github.com/xpadev/ccx-t2/internal/runtime"
	"github.com/xpadev/ccx-t2/internal/tmux"
	"github.com/xpadev/ccx-t2/internal/web"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: ccx serve [--config path] [--web-dir path]")
		os.Exit(2)
	}
	if err := serve(os.Args[2:]); err != nil {
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
