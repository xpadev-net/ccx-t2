package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestCreateContextRejectsCanceledContext(t *testing.T) {
	repoPath := initRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worker-task-001")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	oldExecCommandContext := execCommandContext
	called := false
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		called = true
		return exec.Command("sh", "-c", "true")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	err := CreateContext(ctx, repoPath, "feature/task-001", worktreePath, "HEAD")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateContext canceled error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("git command was started after context was already canceled")
	}
}

func TestHeadRefContextUsesCommandContext(t *testing.T) {
	repoPath := t.TempDir()
	ctx := context.WithValue(context.Background(), struct{}{}, "marker")
	var gotCtx context.Context
	var gotName string
	var gotArgs []string

	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotCtx = ctx
		gotName = name
		gotArgs = append([]string(nil), args...)
		return exec.Command("sh", "-c", "printf abc123")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	got, err := HeadRefContext(ctx, repoPath)
	if err != nil {
		t.Fatalf("HeadRefContext: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("HeadRefContext = %q, want abc123", got)
	}
	if gotCtx != ctx {
		t.Fatal("HeadRefContext did not pass caller context to git command")
	}
	wantArgs := []string{"-C", repoPath, "rev-parse", "HEAD"}
	if gotName != "git" || !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("command = %s %#v, want git %#v", gotName, gotArgs, wantArgs)
	}
}

func TestContextWithDefaultTimeoutKeepsRemoteChecksShorterThanLongParent(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), time.Hour)
	defer cancelParent()

	ctx, cancel := contextWithDefaultTimeout(parent, 5*time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("contextWithDefaultTimeout returned context without deadline")
	}
	now := time.Now()
	if deadline.Before(now) || deadline.After(now.Add(6*time.Second)) {
		t.Fatalf("deadline = %v, want default timeout near 5s", deadline)
	}
}

func TestContextWithDefaultTimeoutHonorsShorterParent(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()

	ctx, cancel := contextWithDefaultTimeout(parent, 5*time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("contextWithDefaultTimeout returned context without deadline")
	}
	now := time.Now()
	if deadline.Before(now) || deadline.After(now.Add(2*time.Second)) {
		t.Fatalf("deadline = %v, want shorter parent deadline", deadline)
	}
}

func TestEnsureBranchCreationSafeAllowsNewWorkerBranch(t *testing.T) {
	repoPath := initRepo(t)

	if err := EnsureBranchCreationSafe(repoPath, "feature/task-001"); err != nil {
		t.Fatalf("EnsureBranchCreationSafe(new branch): %v", err)
	}
}

func TestEnsureBranchCreationSafeRejectsLocalBranch(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "branch", "feature/task-001")

	err := EnsureBranchCreationSafe(repoPath, "feature/task-001")
	if !errors.Is(err, ErrUnsafeBranchCreate) {
		t.Fatalf("EnsureBranchCreationSafe(local branch) error = %v, want ErrUnsafeBranchCreate", err)
	}
}

func TestEnsureBranchCreationSafeRejectsDefaultBranch(t *testing.T) {
	repoPath := initRepo(t)

	err := EnsureBranchCreationSafe(repoPath, "main")
	if !errors.Is(err, ErrUnsafeBranchCreate) {
		t.Fatalf("EnsureBranchCreationSafe(main) error = %v, want ErrUnsafeBranchCreate", err)
	}
}

func TestEnsureBranchCreationSafeRejectsCustomRemoteDefaultBranch(t *testing.T) {
	repoPath := initRepo(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	head := gitOutput(t, repoPath, "rev-parse", "HEAD")
	runGitNoC(t, "init", "--bare", remotePath)
	runGit(t, repoPath, "remote", "add", "origin", remotePath)
	runGit(t, repoPath, "push", "origin", "main")
	runGitNoC(t, "--git-dir", remotePath, "update-ref", "refs/heads/develop", head)
	runGitNoC(t, "--git-dir", remotePath, "symbolic-ref", "HEAD", "refs/heads/develop")

	err := EnsureBranchCreationSafe(repoPath, "develop")
	if !errors.Is(err, ErrUnsafeBranchCreate) {
		t.Fatalf("EnsureBranchCreationSafe(remote default) error = %v, want ErrUnsafeBranchCreate", err)
	}
	if !strings.Contains(err.Error(), "remote default branch") {
		t.Fatalf("EnsureBranchCreationSafe(remote default) error = %v, want remote default branch reason", err)
	}
}

func TestEnsureBranchCreationSafeRejectsRemoteBranchAsPullRequestRisk(t *testing.T) {
	repoPath := initRepo(t)
	remotePath := filepath.Join(t.TempDir(), "origin.git")
	head := gitOutput(t, repoPath, "rev-parse", "HEAD")
	runGitNoC(t, "init", "--bare", remotePath)
	runGit(t, repoPath, "remote", "add", "origin", remotePath)
	runGit(t, repoPath, "push", "origin", "main")
	runGitNoC(t, "--git-dir", remotePath, "update-ref", "refs/heads/feature/task-001", head)

	err := EnsureBranchCreationSafe(repoPath, "feature/task-001")
	if !errors.Is(err, ErrUnsafeBranchCreate) {
		t.Fatalf("EnsureBranchCreationSafe(remote branch) error = %v, want ErrUnsafeBranchCreate", err)
	}
	if !strings.Contains(err.Error(), "exists on a remote") {
		t.Fatalf("EnsureBranchCreationSafe(remote branch) error = %v, want remote branch reason", err)
	}
}

func TestEnsureBranchCreationSafeReportsOriginUnavailable(t *testing.T) {
	repoPath := initRepo(t)
	runGit(t, repoPath, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))

	err := EnsureBranchCreationSafe(repoPath, "feature/task-001")
	if !errors.Is(err, ErrOriginUnavailable) {
		t.Fatalf("EnsureBranchCreationSafe(missing origin) error = %v, want ErrOriginUnavailable", err)
	}
	if branchExists(t, repoPath, "feature/task-001") {
		t.Fatal("feature/task-001 was created, want only preflight validation")
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

func TestDeleteTaskBranchIfSafeSkipsCustomRemoteDefaultBranch(t *testing.T) {
	repoPath := initRepo(t)
	remotePath := filepath.Join(t.TempDir(), "upstream.git")
	runGitNoC(t, "init", "--bare", remotePath)
	runGit(t, repoPath, "remote", "add", "upstream", remotePath)
	runGit(t, repoPath, "branch", "develop")
	runGit(t, repoPath, "push", "upstream", "develop")
	runGitNoC(t, "--git-dir", remotePath, "symbolic-ref", "HEAD", "refs/heads/develop")

	err := DeleteTaskBranchIfSafe(repoPath, "develop", "develop")
	if !errors.Is(err, ErrUnsafeBranchDelete) {
		t.Fatalf("DeleteTaskBranchIfSafe(develop) error = %v, want ErrUnsafeBranchDelete", err)
	}
	if !branchExists(t, repoPath, "develop") {
		t.Fatal("develop was deleted, want preserved")
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
