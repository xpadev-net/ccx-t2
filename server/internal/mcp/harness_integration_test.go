package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
	"github.com/xpadev/ccx-t2/internal/tmux"
	"github.com/xpadev/ccx-t2/internal/worker"
)

const fakeHarnessWait = 30 * time.Second

func TestWorkerHarnessesCompleteThroughMCPIntegration(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not available: %v", err)
	}

	root := t.TempDir()
	harnessCommand := writeFakeHarness(t, root, pythonPath)

	for _, harnessName := range []string{"claude", "codex", "opencode", "cursor-agent"} {
		harnessName := harnessName
		t.Run(harnessName, func(t *testing.T) {
			t.Parallel()

			repoPath := initTestRepo(t)
			worktreeBase := filepath.Join(t.TempDir(), "worktrees")
			if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
				t.Fatalf("mkdir worktree base: %v", err)
			}
			ledgerDir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(ledgerDir, "ledger.md"), filepath.Join(ledgerDir, "archive"))
			taskID := strings.ReplaceAll("task-"+harnessName, "-", "_")
			if err := l.Add(ledger.Task{
				ID:     taskID,
				Title:  "Complete with " + harnessName,
				Status: "unstarted",
				Body:   "exercise fake harness notify flow",
				AllowedFiles: []string{
					"server/internal/mcp",
				},
			}); err != nil {
				t.Fatalf("Add task: %v", err)
			}

			cfg := integrationConfig(repoPath, worktreeBase, harnessName, harnessCommand)
			registry := worker.NewRegistry()
			session := "ccx-t2-test-" + harnessName + "-" + fmt.Sprint(time.Now().UnixNano())
			branch := "phase6/" + harnessName
			worktreePath := filepath.Join(worktreeBase, cfg.Project.Slug+"-"+taskID)
			t.Cleanup(func() {
				_ = exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktreePath).Run()
				_ = exec.Command("git", "-C", repoPath, "branch", "-D", branch).Run()
			})
			if err := tmux.EnsureSession(session); err != nil {
				t.Fatalf("ensure tmux session: %v", err)
			}
			t.Cleanup(func() {
				_ = exec.Command("tmux", "kill-session", "-t", session).Run()
			})

			deps := &Deps{
				Ledger:   l,
				Registry: registry,
				Config:   cfg,
				Session:  session,
			}
			workerServer := NewServer("worker", cfg.Server.McpSecret)
			RegisterWorkerTools(workerServer, deps)
			mux := http.NewServeMux()
			mux.Handle("/mcp/worker", workerServer)
			httpServer := httptest.NewServer(mux)
			t.Cleanup(httpServer.Close)
			deps.BaseURL = httpServer.URL

			got, err := handleSpawnWorker(deps)(context.Background(), map[string]any{
				"task_id":       taskID,
				"branch":        branch,
				"allowed_files": []any{"server/internal/mcp"},
				"harness":       harnessName,
			})
			if err != nil {
				t.Fatalf("spawn_worker: %v", err)
			}
			result, ok := got.(map[string]any)
			if !ok {
				t.Fatalf("spawn_worker result type = %T, want map[string]any", got)
			}
			promptSent, ok := result["prompt_sent"].(bool)
			if !ok {
				t.Fatalf("spawn_worker prompt_sent = %#v, want bool true", result["prompt_sent"])
			}
			if !promptSent {
				t.Fatalf("spawn_worker result = %#v, want prompt_sent true", result)
			}

			waitForCompletedTask(t, l, taskID, harnessName, session, "worker-"+taskID)
			waitForWorkerCleanup(t, registry, session, "worker-"+taskID, worktreePath)
		})
	}
}

func TestWorkerSplitRequestThroughMCPIntegration(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not available: %v", err)
	}

	repoPath := initTestRepo(t)
	worktreeBase := filepath.Join(t.TempDir(), "worktrees")
	if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
		t.Fatalf("mkdir worktree base: %v", err)
	}
	root := t.TempDir()
	harnessName := "codex"
	splitHarnessCommand := writeFakeSplitHarness(t, root, pythonPath)
	completeHarnessCommand := writeFakeHarness(t, root, pythonPath)
	ledgerDir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(ledgerDir, "ledger.md"), filepath.Join(ledgerDir, "archive"))
	const taskID = "task_split"
	if err := l.Add(ledger.Task{
		ID:     taskID,
		Title:  "Split with codex",
		Status: "unstarted",
		Body:   "exercise fake split request flow",
		AllowedFiles: []string{
			"server/internal/mcp",
		},
	}); err != nil {
		t.Fatalf("Add task: %v", err)
	}

	cfg := integrationConfig(repoPath, worktreeBase, harnessName, splitHarnessCommand)
	registry := worker.NewRegistry()
	session := "ccx-t2-test-split-" + fmt.Sprint(time.Now().UnixNano())
	branch := "phase6/split-request"
	worktreePath := filepath.Join(worktreeBase, cfg.Project.Slug+"-"+taskID)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktreePath).Run()
		_ = exec.Command("git", "-C", repoPath, "branch", "-D", branch).Run()
	})
	if err := tmux.EnsureSession(session); err != nil {
		t.Fatalf("ensure tmux session: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", session).Run()
	})

	deps := &Deps{
		Ledger:   l,
		Registry: registry,
		Config:   cfg,
		Session:  session,
	}
	workerServer := NewServer("worker", cfg.Server.McpSecret)
	RegisterWorkerTools(workerServer, deps)
	mux := http.NewServeMux()
	mux.Handle("/mcp/worker", workerServer)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	deps.BaseURL = httpServer.URL

	got, err := handleSpawnWorker(deps)(context.Background(), map[string]any{
		"task_id":       taskID,
		"branch":        branch,
		"allowed_files": []any{"server/internal/mcp"},
		"harness":       harnessName,
	})
	if err != nil {
		t.Fatalf("spawn_worker: %v", err)
	}
	assertPromptSent(t, got, "spawn_worker")

	childID, childAllowed, childForbidden := waitForSplitTask(t, l, taskID, session, "worker-"+taskID)
	waitForWorkerCleanup(t, registry, session, "worker-"+taskID, worktreePath)

	cfg.Harnesses[harnessName] = config.HarnessConfig{
		Command: completeHarnessCommand,
		McpArgs: "--mcp-url {url} --mcp-secret {secret}",
	}
	childBranch := "phase6/split-child"
	childWorktreePath := filepath.Join(worktreeBase, cfg.Project.Slug+"-"+childID)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", childWorktreePath).Run()
		_ = exec.Command("git", "-C", repoPath, "branch", "-D", childBranch).Run()
	})
	got, err = handleSpawnWorker(deps)(context.Background(), map[string]any{
		"task_id":         childID,
		"branch":          childBranch,
		"allowed_files":   stringsToAny(childAllowed),
		"forbidden_files": stringsToAny(childForbidden),
		"harness":         harnessName,
	})
	if err != nil {
		t.Fatalf("spawn child worker: %v", err)
	}
	assertPromptSent(t, got, "spawn child worker")
	waitForCompletedTask(t, l, childID, harnessName, session, "worker-"+childID)
	waitForWorkerCleanup(t, registry, session, "worker-"+childID, childWorktreePath)
}

func integrationConfig(repoPath, worktreeBase, harnessName, harnessCommand string) *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{
			Slug:              "phase6",
			RepoPath:          repoPath,
			WorktreeBase:      worktreeBase,
			ValidationCommand: "go test ./...",
		},
		Server: config.ServerConfig{McpSecret: "phase6-secret"},
		WorkerHarnesses: []string{
			harnessName,
		},
		Harnesses: map[string]config.HarnessConfig{
			harnessName: {
				Command: harnessCommand,
				McpArgs: "--mcp-url {url} --mcp-secret {secret}",
			},
		},
	}
}

func writeFakeHarness(t *testing.T, dir, pythonPath string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-harness")
	script := "#!" + pythonPath + `
import argparse
import json
import re
import select
import sys
import time
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--mcp-url", required=True)
parser.add_argument("--mcp-secret", required=True)
args = parser.parse_args()

prompt = ""
deadline = time.monotonic() + ` + fmt.Sprintf("%.1f", fakeHarnessWait.Seconds()) + `
while time.monotonic() < deadline:
    ready, _, _ = select.select([sys.stdin], [], [], 0.2)
    if not ready:
        continue
    line = sys.stdin.readline()
    if not line:
        break
    prompt += line
    if "When complete: call notify" in line:
        break

task_match = re.search(r"^Task ID: (.+)$", prompt, re.MULTILINE)
worker_match = re.search(r"^Worker ID: (.+)$", prompt, re.MULTILINE)
if task_match is None or worker_match is None:
    print("fake harness could not parse worker prompt", file=sys.stderr)
    print(prompt, file=sys.stderr)
    raise SystemExit(2)
task_id = task_match.group(1)
worker_id = worker_match.group(1)
body = json.dumps({
    "jsonrpc": "2.0",
    "id": "fake-harness",
    "method": "tools/call",
    "params": {
        "name": "notify",
        "arguments": {
            "type": "completed",
            "payload": {
                "task_id": task_id,
                "worker_id": worker_id,
                "pr_url": "https://example.test/pull/fake",
                "merge_commit": "fake-merge"
            }
        }
    }
}).encode()
request = urllib.request.Request(
    args.mcp_url,
    data=body,
    headers={
        "Authorization": "Bearer " + args.mcp_secret,
        "Content-Type": "application/json",
    },
)
try:
    with urllib.request.urlopen(request, timeout=5) as response:
        payload = json.loads(response.read().decode())
except Exception as exc:
    print("fake harness notify request failed:", exc, file=sys.stderr)
    raise SystemExit(3)
if "error" in payload:
    raise SystemExit(payload["error"])
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	return path
}

func writeFakeSplitHarness(t *testing.T, dir, pythonPath string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-split-harness")
	script := "#!" + pythonPath + `
import argparse
import json
import re
import select
import sys
import time
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--mcp-url", required=True)
parser.add_argument("--mcp-secret", required=True)
args = parser.parse_args()

prompt = ""
deadline = time.monotonic() + ` + fmt.Sprintf("%.1f", fakeHarnessWait.Seconds()) + `
while time.monotonic() < deadline:
    ready, _, _ = select.select([sys.stdin], [], [], 0.2)
    if not ready:
        continue
    line = sys.stdin.readline()
    if not line:
        break
    prompt += line
    if "If you need to split: call notify" in line:
        break

task_match = re.search(r"^Task ID: (.+)$", prompt, re.MULTILINE)
worker_match = re.search(r"^Worker ID: (.+)$", prompt, re.MULTILINE)
if task_match is None or worker_match is None:
    print("fake split harness could not parse worker prompt", file=sys.stderr)
    print(prompt, file=sys.stderr)
    raise SystemExit(2)
task_id = task_match.group(1)
worker_id = worker_match.group(1)
body = json.dumps({
    "jsonrpc": "2.0",
    "id": "fake-split-harness",
    "method": "tools/call",
    "params": {
        "name": "notify",
        "arguments": {
            "type": "split_request",
            "payload": {
                "task_id": task_id,
                "worker_id": worker_id,
                "reason": "needs smaller slices",
                "proposed_slices": [
                    {
                        "title": "Child API",
                        "description": "Implement API slice",
                        "allowed_files": ["server/internal/mcp"],
                        "forbidden_files": ["server/internal/mcp/legacy.go"]
                    },
                    {
                        "title": "Child UI",
                        "description": "Implement UI slice",
                        "allowed_files": ["web/src"],
                        "forbidden_files": []
                    }
                ]
            }
        }
    }
}).encode()
request = urllib.request.Request(
    args.mcp_url,
    data=body,
    headers={
        "Authorization": "Bearer " + args.mcp_secret,
        "Content-Type": "application/json",
    },
)
try:
    with urllib.request.urlopen(request, timeout=5) as response:
        payload = json.loads(response.read().decode())
except Exception as exc:
    print("fake split harness notify request failed:", exc, file=sys.stderr)
    raise SystemExit(3)
if "error" in payload:
    raise SystemExit(payload["error"])
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake split harness: %v", err)
	}
	return path
}

func waitForCompletedTask(t *testing.T, l *ledger.Ledger, taskID, harnessName, session, workerID string) {
	t.Helper()
	deadline := time.Now().Add(fakeHarnessWait)
	var last ledger.Task
	for time.Now().Before(deadline) {
		tasks, err := l.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		for _, task := range tasks {
			if task.ID != taskID {
				continue
			}
			last = task
			if task.Status == "completed" {
				if task.Harness != "" || task.WorkerID != "" {
					t.Fatalf("completed task retained runtime fields: %#v", task)
				}
				if task.PrURL != "https://example.test/pull/fake" {
					t.Fatalf("pr_url = %q, want fake PR URL", task.PrURL)
				}
				if !strings.Contains(task.Body, "<!-- merge_commit: fake-merge -->") {
					t.Fatalf("task body missing merge commit comment: %q", task.Body)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("task %s did not complete through %s harness, last state: %#v\ntmux pane:\n%s",
		taskID, harnessName, last, capturePane(session, workerID))
}

func waitForSplitTask(t *testing.T, l *ledger.Ledger, taskID, session, workerID string) (string, []string, []string) {
	t.Helper()
	deadline := time.Now().Add(fakeHarnessWait)
	var tasks []ledger.Task
	for time.Now().Before(deadline) {
		var err error
		tasks, err = l.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		parentIndex := -1
		for i, task := range tasks {
			if task.ID == taskID {
				parentIndex = i
				break
			}
		}
		if parentIndex >= 0 && tasks[parentIndex].Status == "split" && len(tasks) == 3 {
			parent := tasks[parentIndex]
			if parent.WorkerID != "" || parent.Branch != "" || parent.Harness != "" {
				t.Fatalf("split parent retained runtime fields: %#v", parent)
			}
			if parent.Reason != "needs smaller slices" {
				t.Fatalf("split reason = %q, want needs smaller slices", parent.Reason)
			}
			childID, childAllowed, childForbidden := assertChildTask(t, tasks, "Child API", "Implement API slice", []string{"server/internal/mcp"}, []string{"server/internal/mcp/legacy.go"})
			assertChildTask(t, tasks, "Child UI", "Implement UI slice", []string{"web/src"}, []string{})
			return childID, childAllowed, childForbidden
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("task %s did not split, tasks=%#v\ntmux pane:\n%s", taskID, tasks, capturePane(session, workerID))
	return "", nil, nil
}

func assertChildTask(t *testing.T, tasks []ledger.Task, title, body string, allowed, forbidden []string) (string, []string, []string) {
	t.Helper()
	for _, task := range tasks {
		if task.Title != title {
			continue
		}
		if task.Status != "unstarted" || task.Body != body {
			t.Fatalf("child %q = %#v, want unstarted body %q", title, task, body)
		}
		if strings.Join(task.AllowedFiles, "\x00") != strings.Join(allowed, "\x00") {
			t.Fatalf("child %q allowed = %#v, want %#v", title, task.AllowedFiles, allowed)
		}
		if strings.Join(task.ForbiddenFiles, "\x00") != strings.Join(forbidden, "\x00") {
			t.Fatalf("child %q forbidden = %#v, want %#v", title, task.ForbiddenFiles, forbidden)
		}
		return task.ID, append([]string(nil), task.AllowedFiles...), append([]string(nil), task.ForbiddenFiles...)
	}
	t.Fatalf("child %q not found in %#v", title, tasks)
	return "", nil, nil
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func assertPromptSent(t *testing.T, got any, label string) {
	t.Helper()
	result, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("%s result type = %T, want map[string]any", label, got)
	}
	if result["prompt_sent"] != true {
		t.Fatalf("%s result = %#v, want prompt_sent true", label, got)
	}
}

func waitForWorkerCleanup(t *testing.T, registry *worker.Registry, session, workerID, worktreePath string) {
	t.Helper()
	deadline := time.Now().Add(fakeHarnessWait)
	for time.Now().Before(deadline) {
		_, registered := registry.Get(workerID)
		_, statErr := os.Stat(worktreePath)
		alive, aliveErr := tmux.IsWindowAlive(session, workerID)
		if aliveErr != nil {
			t.Fatalf("check tmux window: %v", aliveErr)
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			t.Fatalf("unexpected stat error for worktree: %v", statErr)
		}
		if !registered && os.IsNotExist(statErr) && !alive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, registered := registry.Get(workerID)
	_, statErr := os.Stat(worktreePath)
	alive, aliveErr := tmux.IsWindowAlive(session, workerID)
	t.Fatalf("worker cleanup did not finish: registered=%v worktree_stat=%v window_alive=%v window_err=%v",
		registered, statErr, alive, aliveErr)
}

func capturePane(session, window string) string {
	out, err := exec.Command("tmux", "capture-pane", "-t", session+":"+window, "-p", "-S", "-200").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out)
}
