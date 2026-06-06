package mcp

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/xpadev/ccx-t2/internal/ledger"
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
