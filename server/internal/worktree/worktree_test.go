package worktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateCreatesDedicatedBranchWorktree(t *testing.T) {
	repoPath := initRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worker-task-001")
	baseRef := gitOutput(t, repoPath, "rev-parse", "HEAD")

	if err := Create(repoPath, "feature/task-001", worktreePath, baseRef); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktreePath, ".git")); err != nil {
		t.Fatalf("worktree .git stat: %v", err)
	}
	if got := gitOutput(t, worktreePath, "branch", "--show-current"); got != "feature/task-001" {
		t.Fatalf("worktree branch = %q, want feature/task-001", got)
	}
	if got := gitOutput(t, repoPath, "branch", "--show-current"); got != "main" {
		t.Fatalf("parent repo branch = %q, want main", got)
	}
}

func TestCreateRejectsExistingWorktreePath(t *testing.T) {
	repoPath := initRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worker-task-001")
	if err := os.Mkdir(worktreePath, 0o755); err != nil {
		t.Fatalf("mkdir worktree path: %v", err)
	}

	err := Create(repoPath, "feature/task-001", worktreePath, "HEAD")
	if err == nil {
		t.Fatal("Create error = nil, want existing worktree path error")
	}
}

func TestCreateRejectsDuplicateBranch(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "branch", "feature/task-001")
	worktreePath := filepath.Join(t.TempDir(), "worker-task-001")

	err := Create(repoPath, "feature/task-001", worktreePath, "HEAD")
	if err == nil {
		t.Fatal("Create error = nil, want duplicate branch error")
	}
}

func TestDeleteTaskBranchIfSafeDeletesLocalOnlyWorkerBranch(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "branch", "feature/task-001")

	if err := DeleteTaskBranchIfSafe(repoPath, "feature/task-001", "task-001"); err != nil {
		t.Fatalf("DeleteTaskBranchIfSafe: %v", err)
	}
	if branchExists(t, repoPath, "feature/task-001") {
		t.Fatal("feature/task-001 still exists, want deleted")
	}
}

func TestDeleteTaskBranchIfSafeSkipsDefaultBranch(t *testing.T) {
	repoPath := initRepo(t)

	err := DeleteTaskBranchIfSafe(repoPath, "main", "main")
	if !errors.Is(err, ErrUnsafeBranchDelete) {
		t.Fatalf("DeleteTaskBranchIfSafe(main) error = %v, want ErrUnsafeBranchDelete", err)
	}
	if !branchExists(t, repoPath, "main") {
		t.Fatal("main was deleted, want preserved")
	}
}

func TestDeleteTaskBranchIfSafeSkipsNonTaskScopedBranch(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "branch", "develop")

	err := DeleteTaskBranchIfSafe(repoPath, "develop", "task-001")
	if !errors.Is(err, ErrUnsafeBranchDelete) {
		t.Fatalf("DeleteTaskBranchIfSafe(develop) error = %v, want ErrUnsafeBranchDelete", err)
	}
	if !branchExists(t, repoPath, "develop") {
		t.Fatal("develop was deleted, want preserved")
	}
}

func TestDeleteTaskBranchIfSafeDeletesLegacyNamespacedLocalBranch(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "branch", "feature/my-work")

	if err := DeleteTaskBranchIfSafe(repoPath, "feature/my-work", "task-001"); err != nil {
		t.Fatalf("DeleteTaskBranchIfSafe(legacy branch): %v", err)
	}
	if branchExists(t, repoPath, "feature/my-work") {
		t.Fatal("feature/my-work still exists, want deleted")
	}
}

func TestDeleteTaskBranchIfSafeRequiresTaskIDBoundary(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "branch", "feature/task-0012")

	err := DeleteTaskBranchIfSafe(repoPath, "feature/task-0012", "task-001")
	if !errors.Is(err, ErrUnsafeBranchDelete) {
		t.Fatalf("DeleteTaskBranchIfSafe(prefix task ID) error = %v, want ErrUnsafeBranchDelete", err)
	}
	if !branchExists(t, repoPath, "feature/task-0012") {
		t.Fatal("feature/task-0012 was deleted, want preserved")
	}
}

func TestDeleteTaskBranchIfSafeRemoteRefRequiresExactBranchName(t *testing.T) {
	repoPath := initRepo(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	runGitNoC(t, "init", "--bare", remotePath)
	runGit(t, repoPath, "remote", "add", "origin", remotePath)
	runGit(t, repoPath, "branch", "feature/task-001")
	runGit(t, repoPath, "push", "origin", "feature/task-001")
	runGit(t, repoPath, "branch", "task-001")

	if err := DeleteTaskBranchIfSafe(repoPath, "task-001", "task-001"); err != nil {
		t.Fatalf("DeleteTaskBranchIfSafe(short branch): %v", err)
	}
	if branchExists(t, repoPath, "task-001") {
		t.Fatal("task-001 still exists, want deleted")
	}
	if !branchExists(t, repoPath, "feature/task-001") {
		t.Fatal("feature/task-001 was deleted, want preserved")
	}
}

func TestDeleteTaskBranchIfSafeReportsOriginUnavailable(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
	runGit(t, repoPath, "branch", "feature/task-001")

	err := DeleteTaskBranchIfSafe(repoPath, "feature/task-001", "task-001")
	if !errors.Is(err, ErrOriginUnavailable) {
		t.Fatalf("DeleteTaskBranchIfSafe(missing origin) error = %v, want ErrOriginUnavailable", err)
	}
	if !branchExists(t, repoPath, "feature/task-001") {
		t.Fatal("feature/task-001 was deleted, want preserved")
	}
}

func TestDeleteTaskBranchIfSafeSkipsRemoteBranchAsPullRequestRisk(t *testing.T) {
	repoPath := initRepo(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	runGitNoC(t, "init", "--bare", remotePath)
	runGit(t, repoPath, "remote", "add", "origin", remotePath)
	runGit(t, repoPath, "branch", "feature/task-001")
	runGit(t, repoPath, "push", "origin", "feature/task-001")

	err := DeleteTaskBranchIfSafe(repoPath, "feature/task-001", "task-001")
	if !errors.Is(err, ErrUnsafeBranchDelete) {
		t.Fatalf("DeleteTaskBranchIfSafe(remote branch) error = %v, want ErrUnsafeBranchDelete", err)
	}
	if !branchExists(t, repoPath, "feature/task-001") {
		t.Fatal("feature/task-001 was deleted, want preserved")
	}
}

func TestDeleteTaskBranchIfSafeSkipsBranchCheckedOutInWorktree(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "branch", "feature/task-001")
	otherWorktreePath := filepath.Join(t.TempDir(), "other-worktree")
	runGit(t, repoPath, "worktree", "add", otherWorktreePath, "feature/task-001")

	err := DeleteTaskBranchIfSafe(repoPath, "feature/task-001", "task-001")
	if !errors.Is(err, ErrUnsafeBranchDelete) {
		t.Fatalf("DeleteTaskBranchIfSafe(checked out) error = %v, want ErrUnsafeBranchDelete", err)
	}
	if !branchExists(t, repoPath, "feature/task-001") {
		t.Fatal("feature/task-001 was deleted, want preserved")
	}
}

func TestDeleteTaskBranchIfSafeReportsCleanupFailure(t *testing.T) {
	err := DeleteTaskBranchIfSafe(t.TempDir(), "feature/task-001", "task-001")
	if err == nil {
		t.Fatal("DeleteTaskBranchIfSafe(non-repo) error = nil, want failure")
	}
}

func initRepo(t *testing.T) string {
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

func branchExists(t *testing.T, repoPath, branch string) bool {
	t.Helper()
	err := exec.Command("git", "-C", repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run()
	if err == nil {
		return true
	}
	exitErr, ok := err.(*exec.ExitError)
	if ok && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("check branch %s: %v", branch, err)
	return false
}
