package mcp

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
	"github.com/xpadev/ccx-t2/internal/worker"
)

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
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt does not contain %q:\n%s", want, prompt)
		}
	}
}

func TestHandleNotifyCompletedRejectsStaleWorkerIDUnderLedgerLock(t *testing.T) {
	testHandleNotifyRejectsStaleWorkerID(t, "completed", map[string]any{
		"pr_url":       "https://example.test/pr/1",
		"merge_commit": "abc123",
	})
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

	payload := map[string]any{
		"task_id":   "task-001",
		"worker_id": "worker-stale",
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
	if !strings.Contains(err.Error(), "worker \"worker-stale\" is not assigned") {
		t.Fatalf("handleNotify(%s) error = %v, want stale worker ownership error", notifyType, err)
	}

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tasks[0].Status != "in_progress" || tasks[0].Body != "current body" || tasks[0].WorkerID != "worker-current" {
		t.Fatalf("task changed after stale notify rejection: %#v", tasks[0])
	}
	if len(tasks) != 1 {
		t.Fatalf("stale notify left child tasks behind: %#v", tasks)
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
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
