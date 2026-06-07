package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
	"github.com/xpadev/ccx-t2/internal/worker"
)

func TestGetTasksIncludesBody(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{
		ID:           "task-001",
		Title:        "Build API",
		Status:       "unstarted",
		AllowedFiles: []string{"server/internal/web/**"},
		Body:         "Implement read endpoints.",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performRequest(New(Deps{Ledger: l}), http.MethodGet, "/api/tasks")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var tasks []taskResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].Body != "Implement read endpoints." {
		t.Fatalf("Body = %q, want ledger markdown body", tasks[0].Body)
	}
	if len(tasks[0].AllowedFiles) != 1 || tasks[0].AllowedFiles[0] != "server/internal/web/**" {
		t.Fatalf("AllowedFiles = %#v, want server/internal/web/**", tasks[0].AllowedFiles)
	}
}

func TestGetWorkersReturnsSortedSnapshot(t *testing.T) {
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-b", TaskID: "task-002", Harness: "codex", StartedAt: time.Unix(2, 0).UTC()})
	registry.Register(worker.Info{WorkerID: "worker-a", TaskID: "task-001", Harness: "codex", StartedAt: time.Unix(1, 0).UTC()})

	resp := performRequest(New(Deps{Registry: registry}), http.MethodGet, "/api/workers")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var workers []workerResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &workers); err != nil {
		t.Fatalf("decode workers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("len(workers) = %d, want 2", len(workers))
	}
	if got := []string{workers[0].WorkerID, workers[1].WorkerID}; got[0] != "worker-a" || got[1] != "worker-b" {
		t.Fatalf("worker order = %#v, want worker-a, worker-b", got)
	}
}

func TestGetHarnessesUsesConfiguredWorkerHarnesses(t *testing.T) {
	cfg := testConfig()
	cfg.Harnesses["worker"] = config.HarnessConfig{
		Command: "sh",
		McpArgs: "--mcp-url {url} --token nested-secret-value",
	}

	resp := performRequest(New(Deps{Config: cfg}), http.MethodGet, "/api/harnesses")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	for _, secret := range []string{"nested-secret-value", "mcp_args", "McpArgs", "token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("harness response leaked %q in %s", secret, body)
		}
	}

	var harnesses []struct {
		Name      string         `json:"name"`
		Available bool           `json:"available"`
		Usage     map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &harnesses); err != nil {
		t.Fatalf("decode harnesses: %v", err)
	}
	if len(harnesses) != 1 {
		t.Fatalf("len(harnesses) = %d, want 1", len(harnesses))
	}
	if harnesses[0].Name != "worker" || !harnesses[0].Available {
		t.Fatalf("harness = %#v, want available worker", harnesses[0])
	}
	if harnesses[0].Usage["command"] != "sh" {
		t.Fatalf("usage command = %#v, want sh", harnesses[0].Usage["command"])
	}
}

func TestGetConfigRedactsSecrets(t *testing.T) {
	cfg := testConfig()
	cfg.Server.McpSecret = "mcp-secret-value"
	cfg.GitHub.Token = "github-token-value"
	cfg.Project.ValidationCommand = "GITHUB_TOKEN=nested-validation-secret go test ./..."
	cfg.Harnesses["worker"] = config.HarnessConfig{
		Command: "sh",
		McpArgs: "--mcp-url {url} --token nested-secret-value --secret {secret}",
	}

	resp := performRequest(New(Deps{Config: cfg}), http.MethodGet, "/api/config")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	for _, secret := range []string{"mcp-secret-value", "github-token-value", "nested-secret-value", "nested-validation-secret", "mcp_secret", "mcp_args", "McpArgs", "token", "validation_command"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response leaked %q in %s", secret, body)
		}
	}
	for _, want := range []string{"repo_path", "worktree_base", "heartbeat_interval"} {
		if !strings.Contains(body, want) {
			t.Fatalf("config response = %s, want snake_case key %q", body, want)
		}
	}

	var cfgResp configResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &cfgResp); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfgResp.Server.Port != 9090 {
		t.Fatalf("server.port = %d, want 9090", cfgResp.Server.Port)
	}
	if cfgResp.GitHub.Owner != "xpadev-net" || cfgResp.GitHub.Repo != "ccx-t2" {
		t.Fatalf("github = %#v, want owner/repo", cfgResp.GitHub)
	}
	if got := cfgResp.Harnesses["worker"].Command; got != "sh" {
		t.Fatalf("worker harness command = %q, want sh", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	resp := performRequest(New(Deps{}), http.MethodPost, "/api/tasks")
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusMethodNotAllowed)
	}
	if allow := resp.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

func newTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.md")
	if err := os.WriteFile(ledgerPath, nil, 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	return ledger.NewLedger(ledgerPath, filepath.Join(dir, "archive"))
}

func testConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{
			Slug:              "ccx-t2",
			RepoPath:          "/repo",
			WorktreeBase:      "/worktrees",
			ValidationCommand: "go test ./...",
		},
		Server: config.ServerConfig{
			Port: 9090,
		},
		Orchestrator: config.OrchestratorConfig{
			Harness:           "orchestrator",
			HeartbeatInterval: time.Minute,
			Timeout:           30 * time.Minute,
		},
		WorkerHarnesses: []string{"worker"},
		Harnesses: map[string]config.HarnessConfig{
			"worker": {
				Command: "sh",
				McpArgs: "--mcp-url {url}",
			},
			"orchestrator": {
				Command: "sh",
				McpArgs: "--mcp-url {url}",
			},
		},
		GitHub: config.GitHubConfig{
			Token: "github-token",
			Owner: "xpadev-net",
			Repo:  "ccx-t2",
		},
	}
}

func performRequest(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}
