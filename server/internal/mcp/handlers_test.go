package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
	runtimepkg "github.com/xpadev/ccx-t2/internal/runtime"
	"github.com/xpadev/ccx-t2/internal/worker"
)

type recordingNotifyTrigger struct {
	reasons   []string
	onTrigger func()
	err       error
}

func (r *recordingNotifyTrigger) Trigger(_ context.Context, reason string) error {
	r.reasons = append(r.reasons, reason)
	if r.onTrigger != nil {
		r.onTrigger()
	}
	return r.err
}

func TestBuildHarnessCommandPreservesExpandedMCPValuesAsSingleArgs(t *testing.T) {
	tokens, err := buildMCPTokens(
		"--mcp-url {url} --mcp-secret {secret} --header 'Authorization: Bearer {secret}'",
		"http://localhost:8080/mcp/worker",
		"secret with spaces; echo 'nope'",
	)
	if err != nil {
		t.Fatalf("buildMCPTokens() error = %v", err)
	}

	command := buildHarnessCommand("codex", tokens)
	got, err := shellquote.Split(command)
	if err != nil {
		t.Fatalf("generated command is not valid shell syntax: %v", err)
	}

	want := []string{
		"codex",
		"--mcp-url",
		"http://localhost:8080/mcp/worker",
		"--mcp-secret",
		"secret with spaces; echo 'nope'",
		"--header",
		"Authorization: Bearer secret with spaces; echo 'nope'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generated args mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestBuildMCPTokensRejectsInvalidTemplateShellSyntax(t *testing.T) {
	_, err := buildMCPTokens("--mcp-url '{url}", "http://localhost:8080/mcp/worker", "")
	if err == nil {
		t.Fatal("buildMCPTokens() error = nil, want invalid shell syntax error")
	}
}

func TestRegisterWorkerToolsNotifySchemaRequiresWorkerID(t *testing.T) {
	s := NewServer("worker", "")
	RegisterWorkerTools(s, &Deps{})

	var notifyDef *ToolDef
	for i := range s.toolDefs {
		if s.toolDefs[i].Name == "notify" {
			notifyDef = &s.toolDefs[i]
			break
		}
	}
	if notifyDef == nil {
		t.Fatal("notify tool not registered")
	}
	schema := notifyDef.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	payload := props["payload"].(map[string]any)
	required := payload["required"].([]string)
	want := []string{"task_id", "worker_id"}
	if !reflect.DeepEqual(required, want) {
		t.Fatalf("notify payload required = %#v, want %#v", required, want)
	}
}

func TestRegisterWorkerToolsNotifyTriggersOrchestratorOnce(t *testing.T) {
	cases := []struct {
		name       string
		notifyType string
		extra      map[string]any
		wantReason string
	}{
		{
			name:       "completed",
			notifyType: "completed",
			extra: map[string]any{
				"pr_url":       "https://example.test/pr/1",
				"merge_commit": "abc123def456",
			},
			wantReason: "worker completed: task-001",
		},
		{
			name:       "blocked",
			notifyType: "blocked",
			extra: map[string]any{
				"reason": "blocked by dependency",
			},
			wantReason: "worker blocked: task-001",
		},
		{
			name:       "split_request",
			notifyType: "split_request",
			extra: map[string]any{
				"reason": "needs decomposition",
				"proposed_slices": []any{
					map[string]any{
						"title":       "Child",
						"description": "child body",
					},
				},
			},
			wantReason: "worker split_request: task-001",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
			if err := l.Add(ledger.Task{
				ID:       "task-001",
				Title:    "Task",
				Status:   "in_progress",
				WorkerID: "worker-current",
				Body:     "current body",
			}); err != nil {
				t.Fatalf("Add: %v", err)
			}

			trigger := &recordingNotifyTrigger{}
			trigger.onTrigger = func() {
				t.Helper()
				tasks, err := l.Load()
				if err != nil {
					t.Fatalf("Load during trigger: %v", err)
				}
				var parent *ledger.Task
				for i := range tasks {
					if tasks[i].ID == "task-001" {
						parent = &tasks[i]
						break
					}
				}
				if parent == nil {
					t.Fatalf("task-001 missing during trigger: %#v", tasks)
				}
				switch tc.notifyType {
				case "completed":
					if parent.Status != "completed" || parent.WorkerID != "" || parent.PrURL != "https://example.test/pr/1" {
						t.Fatalf("task during completed trigger = %#v, want completed mutation applied", parent)
					}
				case "blocked":
					if parent.Status != "blocked" || parent.WorkerID != "worker-current" || parent.Reason != "blocked by dependency" {
						t.Fatalf("task during blocked trigger = %#v, want blocked mutation applied", parent)
					}
				case "split_request":
					if parent.Status != "split" || parent.WorkerID != "" || len(tasks) != 2 {
						t.Fatalf("tasks during split trigger = %#v, want split parent and one child", tasks)
					}
				}
			}
			deps := testMCPDeps(dir, l)
			deps.NotifyTrigger = trigger
			s := NewServer("worker", "")
			RegisterWorkerTools(s, deps)

			payload := map[string]any{
				"project_slug": "proj",
				"task_id":      "task-001",
				"worker_id":    "worker-current",
			}
			for k, v := range tc.extra {
				payload[k] = v
			}
			call := map[string]any{
				"jsonrpc": "2.0",
				"id":      tc.name,
				"method":  "tools/call",
				"params": map[string]any{
					"name": "notify",
					"arguments": map[string]any{
						"type":    tc.notifyType,
						"payload": payload,
					},
				},
			}
			body, err := json.Marshal(call)
			if err != nil {
				t.Fatalf("Marshal call: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/mcp/worker", bytes.NewReader(body))
			rr := httptest.NewRecorder()

			s.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response is not JSON: %v\n%s", err, rr.Body.String())
			}
			if errPayload, ok := resp["error"]; ok {
				t.Fatalf("notify returned error: %#v", errPayload)
			}
			if len(trigger.reasons) != 1 {
				t.Fatalf("trigger calls = %#v, want exactly one call", trigger.reasons)
			}
			if trigger.reasons[0] != tc.wantReason {
				t.Fatalf("trigger reason = %q, want %q", trigger.reasons[0], tc.wantReason)
			}
		})
	}
}

func TestRegisterWorkerToolsNotifySucceedsWhenPostMutationTriggerFails(t *testing.T) {
	dir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	if err := l.Add(ledger.Task{
		ID:       "task-001",
		Title:    "Task",
		Status:   "in_progress",
		WorkerID: "worker-current",
		Body:     "current body",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	trigger := &recordingNotifyTrigger{err: errors.New("trigger unavailable")}
	deps := testMCPDeps(dir, l)
	deps.NotifyTrigger = trigger
	s := NewServer("worker", "")
	RegisterWorkerTools(s, deps)

	call := map[string]any{
		"jsonrpc": "2.0",
		"id":      "trigger-error",
		"method":  "tools/call",
		"params": map[string]any{
			"name": "notify",
			"arguments": map[string]any{
				"type": "completed",
				"payload": map[string]any{
					"task_id":      "task-001",
					"worker_id":    "worker-current",
					"pr_url":       "https://example.test/pr/1",
					"merge_commit": "abc123def456",
				},
			},
		},
	}
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("Marshal call: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp/worker", bytes.NewReader(body))
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rr.Body.String())
	}
	if errPayload, ok := resp["error"]; ok {
		t.Fatalf("notify returned error after committed mutation: %#v", errPayload)
	}
	if len(trigger.reasons) != 1 || trigger.reasons[0] != "worker completed: task-001" {
		t.Fatalf("trigger calls = %#v, want one attempted completion trigger", trigger.reasons)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Status != "completed" || tasks[0].WorkerID != "" {
		t.Fatalf("tasks = %#v, want completed mutation preserved", tasks)
	}
}

func TestValidateGitBranchNameRejectsInvalidBranch(t *testing.T) {
	if err := validateGitBranchName("feature/ok"); err != nil {
		t.Fatalf("validateGitBranchName(valid) error = %v", err)
	}
	if err := validateGitBranchName("feature..bad"); err == nil {
		t.Fatal("validateGitBranchName(invalid) error = nil, want error")
	}
}

func TestGitBranchExistsDistinguishesAbsentAndExistingBranches(t *testing.T) {
	repoPath := initTestRepo(t)
	runGit(t, repoPath, "branch", "existing")

	exists, err := gitBranchExists(repoPath, "existing")
	if err != nil {
		t.Fatalf("gitBranchExists(existing) error = %v", err)
	}
	if !exists {
		t.Fatal("gitBranchExists(existing) = false, want true")
	}

	exists, err = gitBranchExists(repoPath, "missing")
	if err != nil {
		t.Fatalf("gitBranchExists(missing) error = %v", err)
	}
	if exists {
		t.Fatal("gitBranchExists(missing) = true, want false")
	}

	_, err = gitBranchExists(t.TempDir(), "missing")
	if err == nil {
		t.Fatal("gitBranchExists(non-repo) error = nil, want error")
	}
}

func TestBuildWorkerPromptFromTaskUsesTaskRestrictions(t *testing.T) {
	task := &ledger.Task{
		Title:          "Reloaded title",
		Body:           "Reloaded body",
		AllowedFiles:   []string{"server/internal/mcp"},
		ForbiddenFiles: []string{"server/internal/mcp/old.go"},
	}

	prompt := buildWorkerPromptFromTask(task, "task-1", "worker-task-1", "feature/task-1", "/tmp/wt", "go test ./...")

	for _, want := range []string{
		"Title: Reloaded title",
		"Reloaded body",
		"  - server/internal/mcp\n",
		"  - server/internal/mcp/old.go\n",
		"Work only inside the Worktree path above",
		"Do not rewrite history on a default branch or any branch that has an open pull request",
		"Never force push",
		`Do not call notify(type="completed") until the PR is merged, gh-review-hook has exited 0, and the merge commit is verified.`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt does not contain %q:\n%s", want, prompt)
		}
	}
}

func TestHandleSpawnWorkerRejectsBranchWithoutTaskID(t *testing.T) {
	dir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deps := &Deps{
		Ledger:   l,
		Registry: worker.NewRegistry(),
		Config: &config.Config{
			Project: config.ProjectConfig{
				Slug:         "proj",
				RepoPath:     dir,
				WorktreeBase: filepath.Join(dir, "worktrees"),
			},
		},
	}

	_, err := handleSpawnWorker(deps)(context.Background(), map[string]any{
		"task_id":       "task-001",
		"branch":        "feature/my-work",
		"allowed_files": []any{"server/internal/mcp"},
	})
	if err == nil {
		t.Fatal("handleSpawnWorker error = nil, want task-scoped branch validation error")
	}
	if !strings.Contains(err.Error(), "must include task_id") {
		t.Fatalf("handleSpawnWorker error = %v, want task-scoped branch validation error", err)
	}
}

func TestHandleSpawnWorkerRejectsRemoteBranchCollision(t *testing.T) {
	dir := t.TempDir()
	repoPath := initTestRepo(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	head := gitOutput(t, repoPath, "rev-parse", "HEAD")
	runGitNoC(t, "init", "--bare", remotePath)
	runGit(t, repoPath, "remote", "add", "origin", remotePath)
	runGit(t, repoPath, "push", "origin", "main")
	runGitNoC(t, "--git-dir", remotePath, "update-ref", "refs/heads/feature/task-001-work", head)

	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deps := &Deps{
		Ledger:   l,
		Registry: worker.NewRegistry(),
		Config: &config.Config{
			Project: config.ProjectConfig{
				Slug:         "proj",
				RepoPath:     repoPath,
				WorktreeBase: filepath.Join(dir, "worktrees"),
			},
			WorkerHarnesses: []string{"sh"},
			Harnesses: map[string]config.HarnessConfig{
				"sh": {Command: "sh", McpArgs: "--mcp-url {url}"},
			},
		},
	}

	_, err := handleSpawnWorker(deps)(context.Background(), map[string]any{
		"task_id":       "task-001",
		"branch":        "feature/task-001-work",
		"allowed_files": []any{"server/internal/mcp"},
	})
	if err == nil {
		t.Fatal("handleSpawnWorker error = nil, want remote branch collision error")
	}
	if !strings.Contains(err.Error(), "unsafe to create") || !strings.Contains(err.Error(), "exists on a remote") {
		t.Fatalf("handleSpawnWorker error = %v, want remote branch collision error", err)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Status != "unstarted" || tasks[0].Branch != "" {
		t.Fatalf("task changed after remote branch collision rejection: %#v", tasks)
	}
}

func TestHandleSpawnWorkerRejectsMalformedFileLists(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value any
	}{
		{name: "allowed string", field: "allowed_files", value: "server/internal/mcp"},
		{name: "allowed non-string item", field: "allowed_files", value: []any{float64(1)}},
		{name: "allowed null", field: "allowed_files", value: nil},
		{name: "forbidden string", field: "forbidden_files", value: "server/internal/mcp"},
		{name: "forbidden non-string item", field: "forbidden_files", value: []any{float64(1)}},
		{name: "forbidden null", field: "forbidden_files", value: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
			if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "unstarted"}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			deps := testMCPDeps(dir, l)
			args := map[string]any{
				"task_id":         "task-001",
				"branch":          "feature/task-001-work",
				"allowed_files":   []any{"server/internal/mcp"},
				"forbidden_files": []any{"server/internal/mcp/old.go"},
			}
			args[tc.field] = tc.value

			_, err := handleSpawnWorker(deps)(context.Background(), args)
			if err == nil {
				t.Fatal("handleSpawnWorker error = nil, want file-list type error")
			}
			if !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "array of strings") {
				t.Fatalf("handleSpawnWorker error = %v, want %s array-of-strings error", err, tc.field)
			}
			tasks, err := l.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(tasks) != 1 || tasks[0].Status != "unstarted" || len(tasks[0].AllowedFiles) != 0 || len(tasks[0].ForbiddenFiles) != 0 {
				t.Fatalf("task changed after malformed %s rejection: %#v", tc.field, tasks)
			}
		})
	}
}

func TestCleanupTaskBranchNormalizesRelativeRepoPath(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	if err := exec.Command("git", "init", "-b", "main", repoPath).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	runGit(t, repoPath, "config", "user.email", "test@example.com")
	runGit(t, repoPath, "config", "user.name", "Test User")
	runGit(t, repoPath, "commit", "--allow-empty", "-m", "init")
	runGit(t, repoPath, "branch", "feature/task-001")
	previousWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousWD); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})

	cleanupTaskBranch("repo", "feature/task-001", "task-001")

	exists, err := gitBranchExists(repoPath, "feature/task-001")
	if err != nil {
		t.Fatalf("gitBranchExists: %v", err)
	}
	if exists {
		t.Fatal("feature/task-001 still exists, want cleanup to delete it")
	}
}

func TestProjectScopedListTasksUsesSelectedProject(t *testing.T) {
	dir := t.TempDir()
	worktreeBase := filepath.Join(dir, "worktrees")
	if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
		t.Fatalf("MkdirAll worktrees: %v", err)
	}
	alphaRepo := initTestRepo(t)
	betaRepo := initTestRepo(t)
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Runtime: config.RuntimeConfig{TmuxSession: "ccx-test", WorktreeBase: worktreeBase},
		Orchestrator: config.OrchestratorConfig{
			Harness:           "sh",
			HeartbeatInterval: time.Minute,
			Timeout:           time.Minute,
		},
		WorkerHarnesses: []string{"sh"},
		Harnesses: map[string]config.HarnessConfig{
			"sh": {Command: "sh", McpArgs: "--mcp-url {url}"},
		},
		Projects: map[string]config.ProjectConfig{
			"alpha": {
				RepoPath:     alphaRepo,
				LedgerPath:   filepath.Join(alphaRepo, "tasks", "ledger.md"),
				WorktreeBase: worktreeBase,
			},
			"beta": {
				RepoPath:     betaRepo,
				LedgerPath:   filepath.Join(betaRepo, "tasks", "ledger.md"),
				WorktreeBase: worktreeBase,
			},
		},
	}
	if err := config.Prepare(cfg); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	manager, err := runtimepkg.NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	beta, err := manager.Project("beta")
	if err != nil {
		t.Fatalf("Project beta: %v", err)
	}
	if err := beta.Ledger.Add(ledger.Task{ID: "task-20260101-0001", Title: "Beta", Status: "unstarted"}); err != nil {
		t.Fatalf("Add beta: %v", err)
	}

	result, err := handleListTasks(&Deps{Config: cfg, Manager: manager})(context.Background(), map[string]any{
		"project_slug": "beta",
	})
	if err != nil {
		t.Fatalf("handleListTasks: %v", err)
	}
	tasks := result.([]ledger.Task)
	if len(tasks) != 1 || tasks[0].Title != "Beta" {
		t.Fatalf("tasks = %#v, want beta task", tasks)
	}

	if _, err := handleListTasks(&Deps{Config: cfg, Manager: manager})(context.Background(), map[string]any{
		"project_slug": "missing",
	}); err == nil {
		t.Fatal("handleListTasks missing project error = nil")
	}
}

func TestRegisterWorkerToolsNotifyUsesSelectedProjectTrigger(t *testing.T) {
	dir := t.TempDir()
	worktreeBase := filepath.Join(dir, "worktrees")
	if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
		t.Fatalf("MkdirAll worktrees: %v", err)
	}
	alphaRepo := initTestRepo(t)
	betaRepo := initTestRepo(t)
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Runtime: config.RuntimeConfig{TmuxSession: "ccx-test", WorktreeBase: worktreeBase},
		Orchestrator: config.OrchestratorConfig{
			Harness:           "sh",
			HeartbeatInterval: time.Minute,
			Timeout:           time.Minute,
		},
		WorkerHarnesses: []string{"sh"},
		Harnesses: map[string]config.HarnessConfig{
			"sh": {Command: "sh", McpArgs: "--mcp-url {url}"},
		},
		Projects: map[string]config.ProjectConfig{
			"alpha": {
				RepoPath:     alphaRepo,
				LedgerPath:   filepath.Join(alphaRepo, "tasks", "ledger.md"),
				WorktreeBase: worktreeBase,
			},
			"beta": {
				RepoPath:     betaRepo,
				LedgerPath:   filepath.Join(betaRepo, "tasks", "ledger.md"),
				WorktreeBase: worktreeBase,
			},
		},
	}
	if err := config.Prepare(cfg); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	manager, err := runtimepkg.NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	alpha, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project alpha: %v", err)
	}
	beta, err := manager.Project("beta")
	if err != nil {
		t.Fatalf("Project beta: %v", err)
	}
	if err := alpha.Ledger.Add(ledger.Task{
		ID:       "task-alpha",
		Title:    "Alpha",
		Status:   "in_progress",
		WorkerID: "worker-alpha",
	}); err != nil {
		t.Fatalf("Add alpha: %v", err)
	}
	if err := beta.Ledger.Add(ledger.Task{
		ID:       "task-beta",
		Title:    "Beta",
		Status:   "in_progress",
		WorkerID: "worker-beta",
	}); err != nil {
		t.Fatalf("Add beta: %v", err)
	}

	alphaTrigger := &recordingNotifyTrigger{}
	betaTrigger := &recordingNotifyTrigger{
		onTrigger: func() {
			t.Helper()
			tasks, err := beta.Ledger.Load()
			if err != nil {
				t.Fatalf("Load beta during trigger: %v", err)
			}
			if len(tasks) != 1 || tasks[0].Status != "completed" || tasks[0].WorkerID != "" {
				t.Fatalf("beta tasks during trigger = %#v, want completed beta task", tasks)
			}
		},
	}
	alpha.NotifyTrigger = alphaTrigger
	beta.NotifyTrigger = betaTrigger

	s := NewServer("worker", "")
	RegisterWorkerTools(s, &Deps{Config: cfg, Manager: manager})
	call := map[string]any{
		"jsonrpc": "2.0",
		"id":      "selected-project",
		"method":  "tools/call",
		"params": map[string]any{
			"name": "notify",
			"arguments": map[string]any{
				"type": "completed",
				"payload": map[string]any{
					"project_slug": "beta",
					"task_id":      "task-beta",
					"worker_id":    "worker-beta",
					"pr_url":       "https://example.test/pr/beta",
					"merge_commit": "abc123def456",
				},
			},
		},
	}
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("Marshal call: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp/worker", bytes.NewReader(body))
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rr.Body.String())
	}
	if errPayload, ok := resp["error"]; ok {
		t.Fatalf("notify returned error: %#v", errPayload)
	}
	if len(betaTrigger.reasons) != 1 || betaTrigger.reasons[0] != "worker completed: task-beta" {
		t.Fatalf("beta trigger calls = %#v, want selected project completion trigger", betaTrigger.reasons)
	}
	if len(alphaTrigger.reasons) != 0 {
		t.Fatalf("alpha trigger calls = %#v, want none", alphaTrigger.reasons)
	}
	alphaTasks, err := alpha.Ledger.Load()
	if err != nil {
		t.Fatalf("Load alpha: %v", err)
	}
	if len(alphaTasks) != 1 || alphaTasks[0].Status != "in_progress" || alphaTasks[0].WorkerID != "worker-alpha" {
		t.Fatalf("alpha tasks = %#v, want untouched alpha task", alphaTasks)
	}
}

func TestHandleCreateTaskAcceptsNaturalLanguageRequestWithoutTitle(t *testing.T) {
	dir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	result, err := handleCreateTask(&Deps{Ledger: l})(context.Background(), map[string]any{
		"description": " \n",
		"request":     "Please investigate natural language task intake and create the right slices.",
	})
	if err != nil {
		t.Fatalf("handleCreateTask: %v", err)
	}
	id, ok := result.(map[string]any)["id"].(string)
	if !ok || id == "" {
		t.Fatalf("result = %#v, want generated id", result)
	}

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].Title != "Natural language intake" {
		t.Fatalf("title = %q, want natural-language intake title", tasks[0].Title)
	}
	if !strings.Contains(tasks[0].Body, "Please investigate natural language task intake") {
		t.Fatalf("body = %q, want raw request preserved", tasks[0].Body)
	}
	if len(tasks[0].AllowedFiles) != 0 {
		t.Fatalf("allowed_files = %#v, want none for raw intake", tasks[0].AllowedFiles)
	}
}

func TestHandleCreateTaskRejectsMalformedFileLists(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value any
	}{
		{name: "allowed string", field: "allowed_files", value: "server/internal/mcp"},
		{name: "allowed non-string item", field: "allowed_files", value: []any{float64(1)}},
		{name: "allowed null", field: "allowed_files", value: nil},
		{name: "forbidden string", field: "forbidden_files", value: "server/internal/mcp"},
		{name: "forbidden non-string item", field: "forbidden_files", value: []any{float64(1)}},
		{name: "forbidden null", field: "forbidden_files", value: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
			args := map[string]any{
				"title":           "Task",
				"description":     "Task body",
				"allowed_files":   []any{"server/internal/mcp"},
				"forbidden_files": []any{"server/internal/mcp/old.go"},
			}
			args[tc.field] = tc.value

			_, err := handleCreateTask(&Deps{Ledger: l})(context.Background(), args)
			if err == nil {
				t.Fatal("handleCreateTask error = nil, want file-list type error")
			}
			if !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "array of strings") {
				t.Fatalf("handleCreateTask error = %v, want %s array-of-strings error", err, tc.field)
			}
			tasks, err := l.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(tasks) != 0 {
				t.Fatalf("created task after malformed %s rejection: %#v", tc.field, tasks)
			}
		})
	}
}

func TestHandleSplitTaskRejectsMalformedFileLists(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value any
	}{
		{name: "allowed string", field: "allowed_files", value: "server/internal/mcp"},
		{name: "allowed non-string item", field: "allowed_files", value: []any{float64(1)}},
		{name: "allowed null", field: "allowed_files", value: nil},
		{name: "forbidden string", field: "forbidden_files", value: "server/internal/mcp"},
		{name: "forbidden non-string item", field: "forbidden_files", value: []any{float64(1)}},
		{name: "forbidden null", field: "forbidden_files", value: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
			if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "unstarted", Body: "current body"}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			slice := map[string]any{
				"title":           "Child",
				"description":     "child body",
				"allowed_files":   []any{"server/internal/mcp"},
				"forbidden_files": []any{"server/internal/mcp/old.go"},
			}
			slice[tc.field] = tc.value

			_, err := handleSplitTask(&Deps{Ledger: l})(context.Background(), map[string]any{
				"id":     "task-001",
				"slices": []any{slice},
			})
			if err == nil {
				t.Fatal("handleSplitTask error = nil, want file-list type error")
			}
			if !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "array of strings") {
				t.Fatalf("handleSplitTask error = %v, want %s array-of-strings error", err, tc.field)
			}
			tasks, err := l.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(tasks) != 1 || tasks[0].Status != "unstarted" || tasks[0].Body != "current body" {
				t.Fatalf("task changed after malformed %s rejection: %#v", tc.field, tasks)
			}
		})
	}
}

func TestEnsureWorkerTaskActiveRejectsTerminalOrMissingWorker(t *testing.T) {
	dir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "completed", WorkerID: "worker-done"}); err != nil {
		t.Fatalf("Add completed: %v", err)
	}
	if err := l.Add(ledger.Task{ID: "task-002", Title: "Task", Status: "in_progress", WorkerID: "worker-active"}); err != nil {
		t.Fatalf("Add active: %v", err)
	}

	if err := ensureWorkerTaskActive(l, "worker-active"); err != nil {
		t.Fatalf("ensureWorkerTaskActive(active): %v", err)
	}
	if err := ensureWorkerTaskActive(l, "worker-done"); err == nil {
		t.Fatal("ensureWorkerTaskActive(completed) error = nil, want error")
	}
	if err := ensureWorkerTaskActive(l, "worker-missing"); err == nil {
		t.Fatal("ensureWorkerTaskActive(missing) error = nil, want error")
	}
}

func TestHandleNotifyCompletedRejectsStaleWorkerIDUnderLedgerLock(t *testing.T) {
	testHandleNotifyRejectsStaleWorkerID(t, "completed", map[string]any{
		"pr_url":       "https://example.test/pr/1",
		"merge_commit": "abc123def456",
	})
}

func TestHandleNotifyRejectsMissingOrEmptyWorkerID(t *testing.T) {
	cases := []struct {
		name       string
		notifyType string
		workerID   any
		includeID  bool
		extra      map[string]any
		wantErr    string
	}{
		{
			name:       "completed missing worker",
			notifyType: "completed",
			extra: map[string]any{
				"pr_url":       "https://example.test/pr/1",
				"merge_commit": "abc123def456",
			},
			wantErr: "missing required argument \"worker_id\"",
		},
		{
			name:       "completed empty worker",
			notifyType: "completed",
			workerID:   "",
			includeID:  true,
			extra: map[string]any{
				"pr_url":       "https://example.test/pr/1",
				"merge_commit": "abc123def456",
			},
			wantErr: "worker_id is required",
		},
		{
			name:       "blocked missing worker",
			notifyType: "blocked",
			extra: map[string]any{
				"reason": "blocked",
			},
			wantErr: "missing required argument \"worker_id\"",
		},
		{
			name:       "blocked empty worker",
			notifyType: "blocked",
			workerID:   " ",
			includeID:  true,
			extra: map[string]any{
				"reason": "blocked",
			},
			wantErr: "worker_id is required",
		},
		{
			name:       "split missing worker",
			notifyType: "split_request",
			extra: map[string]any{
				"reason": "split",
				"proposed_slices": []any{
					map[string]any{"title": "Child"},
				},
			},
			wantErr: "missing required argument \"worker_id\"",
		},
		{
			name:       "split empty worker",
			notifyType: "split_request",
			workerID:   "",
			includeID:  true,
			extra: map[string]any{
				"reason": "split",
				"proposed_slices": []any{
					map[string]any{"title": "Child"},
				},
			},
			wantErr: "worker_id is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
			if err := l.Add(ledger.Task{
				ID:       "task-001",
				Title:    "Task",
				Status:   "in_progress",
				WorkerID: "worker-current",
				Body:     "current body",
			}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			deps := testMCPDeps(dir, l)
			payload := map[string]any{"task_id": "task-001"}
			if tc.includeID {
				payload["worker_id"] = tc.workerID
			}
			for k, v := range tc.extra {
				payload[k] = v
			}

			_, err := handleNotify(deps)(context.Background(), map[string]any{
				"type":    tc.notifyType,
				"payload": payload,
			})
			if err == nil {
				t.Fatalf("handleNotify(%s) error = nil, want worker_id error", tc.notifyType)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("handleNotify(%s) error = %v, want %q", tc.notifyType, err, tc.wantErr)
			}
			tasks, err := l.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(tasks) != 1 || tasks[0].Status != "in_progress" || tasks[0].WorkerID != "worker-current" || tasks[0].Body != "current body" {
				t.Fatalf("task changed after invalid worker_id rejection: %#v", tasks)
			}
		})
	}
}

func TestHandleNotifyCompletedRejectsMissingCompletionEvidence(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		wantErr string
	}{
		{
			name: "missing pr_url",
			payload: map[string]any{
				"merge_commit": "abc123",
			},
			wantErr: "pr_url is required",
		},
		{
			name: "missing merge_commit",
			payload: map[string]any{
				"pr_url": "https://example.test/pr/1",
			},
			wantErr: "merge_commit is required",
		},
		{
			name: "multiline pr_url",
			payload: map[string]any{
				"pr_url":       "https://example.test/pr/1\nextra",
				"merge_commit": "abc123def456",
			},
			wantErr: "pr_url must be a single line",
		},
		{
			name: "trailing newline pr_url",
			payload: map[string]any{
				"pr_url":       "https://example.test/pr/1\n",
				"merge_commit": "abc123def456",
			},
			wantErr: "pr_url must be a single line",
		},
		{
			name: "multiline merge_commit",
			payload: map[string]any{
				"pr_url":       "https://example.test/pr/1",
				"merge_commit": "abc123\nextra",
			},
			wantErr: "merge_commit must be a single-line value",
		},
		{
			name: "comment-closing merge_commit",
			payload: map[string]any{
				"pr_url":       "https://example.test/pr/1",
				"merge_commit": "abc123 --> injected",
			},
			wantErr: "merge_commit must be a valid commit hash",
		},
		{
			name: "short merge_commit",
			payload: map[string]any{
				"pr_url":       "https://example.test/pr/1",
				"merge_commit": "abc123",
			},
			wantErr: "merge_commit must be a valid commit hash",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
			if err := l.Add(ledger.Task{
				ID:       "task-001",
				Title:    "Task",
				Status:   "in_progress",
				WorkerID: "worker-task-001",
				Body:     "current body",
			}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			deps := testMCPDeps(dir, l)
			payload := map[string]any{
				"task_id":   "task-001",
				"worker_id": "worker-task-001",
			}
			for k, v := range tc.payload {
				payload[k] = v
			}

			_, err := handleNotify(deps)(context.Background(), map[string]any{
				"type":    "completed",
				"payload": payload,
			})
			if err == nil {
				t.Fatal("handleNotify completed error = nil, want missing completion evidence error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("handleNotify completed error = %v, want %q", err, tc.wantErr)
			}
			tasks, err := l.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(tasks) != 1 || tasks[0].Status != "in_progress" || tasks[0].PrURL != "" || strings.Contains(tasks[0].Body, "merge_commit") {
				t.Fatalf("task changed after rejected completion: %#v", tasks)
			}
		})
	}
}

func TestHandleNotifyBlockedRejectsStaleWorkerIDUnderLedgerLock(t *testing.T) {
	testHandleNotifyRejectsStaleWorkerID(t, "blocked", map[string]any{
		"reason": "blocked",
	})
}

func TestHandleNotifySplitRequestRejectsStaleWorkerIDUnderLedgerLock(t *testing.T) {
	testHandleNotifyRejectsStaleWorkerID(t, "split_request", map[string]any{
		"reason": "split",
		"proposed_slices": []any{
			map[string]any{
				"title":         "Child",
				"allowed_files": []any{"server/internal/mcp"},
			},
		},
	})
}

func TestHandleNotifySplitRequestRejectsMalformedFileLists(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value any
	}{
		{name: "allowed string", field: "allowed_files", value: "server/internal/mcp"},
		{name: "allowed non-string item", field: "allowed_files", value: []any{float64(1)}},
		{name: "allowed null", field: "allowed_files", value: nil},
		{name: "forbidden string", field: "forbidden_files", value: "server/internal/mcp"},
		{name: "forbidden non-string item", field: "forbidden_files", value: []any{float64(1)}},
		{name: "forbidden null", field: "forbidden_files", value: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
			if err := l.Add(ledger.Task{
				ID:       "task-001",
				Title:    "Task",
				Status:   "in_progress",
				WorkerID: "worker-current",
				Body:     "current body",
			}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			slice := map[string]any{
				"title":           "Child",
				"description":     "child body",
				"allowed_files":   []any{"server/internal/mcp"},
				"forbidden_files": []any{"server/internal/mcp/old.go"},
			}
			slice[tc.field] = tc.value

			_, err := handleNotify(testMCPDeps(dir, l))(context.Background(), map[string]any{
				"type": "split_request",
				"payload": map[string]any{
					"task_id":         "task-001",
					"worker_id":       "worker-current",
					"reason":          "needs split",
					"proposed_slices": []any{slice},
				},
			})
			if err == nil {
				t.Fatal("handleNotify split_request error = nil, want file-list type error")
			}
			if !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "array of strings") {
				t.Fatalf("handleNotify split_request error = %v, want %s array-of-strings error", err, tc.field)
			}
			tasks, err := l.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(tasks) != 1 || tasks[0].Status != "in_progress" || tasks[0].WorkerID != "worker-current" || tasks[0].Body != "current body" {
				t.Fatalf("task changed after malformed %s rejection: %#v", tc.field, tasks)
			}
		})
	}
}

func TestHandleNotifyValidOwnerMutatesTask(t *testing.T) {
	cases := []struct {
		name       string
		notifyType string
		extra      map[string]any
		verify     func(t *testing.T, tasks []ledger.Task)
	}{
		{
			name:       "completed",
			notifyType: "completed",
			extra: map[string]any{
				"pr_url":       "https://example.test/pr/1",
				"merge_commit": "abc123def456",
			},
			verify: func(t *testing.T, tasks []ledger.Task) {
				t.Helper()
				if len(tasks) != 1 {
					t.Fatalf("len(tasks) = %d, want 1", len(tasks))
				}
				if tasks[0].Status != "completed" || tasks[0].WorkerID != "" || tasks[0].PrURL != "https://example.test/pr/1" {
					t.Fatalf("completed task = %#v, want completed with cleared worker and PR URL", tasks[0])
				}
				if !strings.Contains(tasks[0].Body, "<!-- merge_commit: abc123def456 -->") {
					t.Fatalf("completed body = %q, want merge commit marker", tasks[0].Body)
				}
			},
		},
		{
			name:       "blocked",
			notifyType: "blocked",
			extra: map[string]any{
				"reason": "blocked by dependency",
			},
			verify: func(t *testing.T, tasks []ledger.Task) {
				t.Helper()
				if len(tasks) != 1 {
					t.Fatalf("len(tasks) = %d, want 1", len(tasks))
				}
				if tasks[0].Status != "blocked" || tasks[0].WorkerID != "worker-current" || tasks[0].Reason != "blocked by dependency" {
					t.Fatalf("blocked task = %#v, want blocked task owned by worker-current with reason", tasks[0])
				}
			},
		},
		{
			name:       "split_request",
			notifyType: "split_request",
			extra: map[string]any{
				"reason": "needs decomposition",
				"proposed_slices": []any{
					map[string]any{
						"title":         "Child",
						"description":   "child body",
						"allowed_files": []any{"server/internal/mcp"},
					},
				},
			},
			verify: func(t *testing.T, tasks []ledger.Task) {
				t.Helper()
				if len(tasks) != 2 {
					t.Fatalf("len(tasks) = %d, want parent and child", len(tasks))
				}
				var parent, child *ledger.Task
				for i := range tasks {
					switch tasks[i].ID {
					case "task-001":
						parent = &tasks[i]
					default:
						child = &tasks[i]
					}
				}
				if parent == nil || child == nil {
					t.Fatalf("tasks after split = %#v, want parent and child", tasks)
				}
				if parent.Status != "split" || parent.WorkerID != "" || parent.Reason != "needs decomposition" {
					t.Fatalf("split parent = %#v, want split with cleared worker and reason", parent)
				}
				if child.Status != "unstarted" || child.Title != "Child" || child.Body != "child body" {
					t.Fatalf("split child = %#v, want unstarted child", child)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
			if err := l.Add(ledger.Task{
				ID:       "task-001",
				Title:    "Task",
				Status:   "in_progress",
				WorkerID: "worker-current",
				Body:     "current body",
			}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			deps := testMCPDeps(dir, l)
			payload := map[string]any{
				"task_id":   "task-001",
				"worker_id": "worker-current",
			}
			for k, v := range tc.extra {
				payload[k] = v
			}

			if _, err := handleNotify(deps)(context.Background(), map[string]any{
				"type":    tc.notifyType,
				"payload": payload,
			}); err != nil {
				t.Fatalf("handleNotify(%s): %v", tc.notifyType, err)
			}
			tasks, err := l.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.verify(t, tasks)
		})
	}
}

func TestHandleArchiveTaskAlreadyArchivedIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Done", Status: "completed"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := l.Archive("task-001", "abc123"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	deps := &Deps{
		Ledger:   l,
		Registry: worker.NewRegistry(),
		Config: &config.Config{
			Project: config.ProjectConfig{
				Slug:         "proj",
				RepoPath:     dir,
				WorktreeBase: dir,
			},
		},
	}

	got, err := handleArchiveTask(deps)(context.Background(), map[string]any{"id": "task-001"})
	if err != nil {
		t.Fatalf("handleArchiveTask already archived: %v", err)
	}
	archived, ok := got.(map[string]any)["archived"].(string)
	if !ok || archived != "task-001" {
		t.Fatalf("handleArchiveTask result = %#v, want archived task-001", got)
	}
}

func TestHandleNotifyCompletedThenArchivePreservesMergeMetadata(t *testing.T) {
	dir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	if err := l.Add(ledger.Task{
		ID:       "task-001",
		Title:    "Done",
		Status:   "in_progress",
		Branch:   "feature/task-001-done",
		WorkerID: "worker-task-001",
		Harness:  "codex",
		Body:     "current body\n<!-- merge_commit: stale -->",
	}); err != nil {
		t.Fatalf("Add completed candidate: %v", err)
	}
	if err := l.Add(ledger.Task{ID: "task-002", Title: "Still active", Status: "unstarted"}); err != nil {
		t.Fatalf("Add active task: %v", err)
	}
	deps := testMCPDeps(dir, l)

	_, err := handleNotify(deps)(context.Background(), map[string]any{
		"type": "completed",
		"payload": map[string]any{
			"task_id":      "task-001",
			"worker_id":    "worker-task-001",
			"pr_url":       "https://example.test/pull/123",
			"merge_commit": "abc123def456",
		},
	})
	if err != nil {
		t.Fatalf("handleNotify completed: %v", err)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load after notify: %v", err)
	}
	var completed *ledger.Task
	for i := range tasks {
		if tasks[i].ID == "task-001" {
			completed = &tasks[i]
			break
		}
	}
	if completed == nil {
		t.Fatalf("completed task missing after notify: %#v", tasks)
	}
	if completed.Status != "completed" || completed.PrURL != "https://example.test/pull/123" {
		t.Fatalf("completed task metadata = %#v, want completed with PR URL", completed)
	}
	if completed.WorkerID != "" || completed.Harness != "" {
		t.Fatalf("completed task retained runtime fields: %#v", completed)
	}
	if !strings.Contains(completed.Body, "<!-- merge_commit: abc123def456 -->") {
		t.Fatalf("completed task missing merge commit comment: %q", completed.Body)
	}
	if strings.Contains(completed.Body, "stale") {
		t.Fatalf("completed task retained stale merge commit marker: %q", completed.Body)
	}

	got, err := handleArchiveTask(deps)(context.Background(), map[string]any{"id": "task-001"})
	if err != nil {
		t.Fatalf("handleArchiveTask: %v", err)
	}
	if got.(map[string]any)["archived"] != "task-001" {
		t.Fatalf("handleArchiveTask result = %#v, want archived task-001", got)
	}
	tasks, err = l.Load()
	if err != nil {
		t.Fatalf("Load after archive: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task-002" {
		t.Fatalf("active ledger tasks after archive = %#v, want only task-002", tasks)
	}
	archived, ok, err := l.ArchivedTask("task-001")
	if err != nil {
		t.Fatalf("ArchivedTask: %v", err)
	}
	if !ok {
		t.Fatal("ArchivedTask ok = false, want archived task")
	}
	if archived.PrURL != "https://example.test/pull/123" || archived.MergeCommit != "abc123def456" {
		t.Fatalf("archived metadata = %#v, want PR URL and merge commit", archived)
	}
	if strings.Contains(archived.Body, "merge_commit") {
		t.Fatalf("archive body retained merge commit comment: %q", archived.Body)
	}
}

func testHandleNotifyRejectsStaleWorkerID(t *testing.T, notifyType string, extraPayload map[string]any) {
	t.Helper()
	dir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	if err := l.Add(ledger.Task{
		ID:       "task-001",
		Title:    "Task",
		Status:   "in_progress",
		WorkerID: "worker-current",
		Body:     "current body",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	deps := &Deps{
		Ledger:   l,
		Registry: worker.NewRegistry(),
		Config: &config.Config{
			Project: config.ProjectConfig{
				Slug:         "proj",
				RepoPath:     dir,
				WorktreeBase: dir,
			},
		},
		Session: "missing-session",
	}

	deps.notifyAfterOwnershipPreflight = func() {
		if _, err := l.UpdateIfStatusesReturnPrev("task-001", []string{"in_progress", "blocked"}, map[string]any{
			"worker_id": "worker-new",
		}); err != nil {
			t.Fatalf("stale worker setup update: %v", err)
		}
	}

	payload := map[string]any{
		"task_id":   "task-001",
		"worker_id": "worker-current",
	}
	for k, v := range extraPayload {
		payload[k] = v
	}
	_, err := handleNotify(deps)(context.Background(), map[string]any{
		"type":    notifyType,
		"payload": payload,
	})
	if err == nil {
		t.Fatalf("handleNotify(%s) error = nil, want stale worker ownership error", notifyType)
	}
	if !strings.Contains(err.Error(), "worker \"worker-current\" is not assigned") {
		t.Fatalf("handleNotify(%s) error = %v, want stale worker ownership error", notifyType, err)
	}

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tasks[0].Status != "in_progress" || tasks[0].Body != "current body" || tasks[0].WorkerID != "worker-new" {
		t.Fatalf("task changed after stale notify rejection: %#v", tasks[0])
	}
	if len(tasks) != 1 {
		t.Fatalf("stale notify left child tasks behind: %#v", tasks)
	}
}

func testMCPDeps(dir string, l *ledger.Ledger) *Deps {
	return &Deps{
		Ledger:   l,
		Registry: worker.NewRegistry(),
		Config: &config.Config{
			Project: config.ProjectConfig{
				Slug:         "proj",
				RepoPath:     dir,
				WorktreeBase: dir,
			},
		},
		Session: "missing-session",
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	repoPath := t.TempDir()
	runGit(t, repoPath, "init", "-b", "main")
	runGit(t, repoPath, "config", "user.email", "test@example.com")
	runGit(t, repoPath, "config", "user.name", "Test User")
	runGit(t, repoPath, "commit", "--allow-empty", "-m", "init")
	return repoPath
}

func runGit(t *testing.T, repoPath string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", repoPath}, args...)
	runGitNoC(t, cmdArgs...)
}

func runGitNoC(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func gitOutput(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", repoPath}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}
