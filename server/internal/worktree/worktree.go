package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Create creates a new git worktree at worktreePath branching from baseRef.
// Both repoPath and worktreePath must be absolute so that os.Stat and git
// resolve them consistently regardless of the process working directory.
func Create(repoPath, branch, worktreePath, baseRef string) error {
	if !filepath.IsAbs(repoPath) {
		return fmt.Errorf("repoPath must be absolute, got: %s", repoPath)
	}
	if !filepath.IsAbs(worktreePath) {
		return fmt.Errorf("worktreePath must be absolute, got: %s", worktreePath)
	}
	if _, err := os.Stat(worktreePath); err == nil {
		return fmt.Errorf("worktree path already exists: %s", worktreePath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking worktree path %s: %w", worktreePath, err)
	}
	return run("git", "-C", repoPath, "worktree", "add", "-b", branch, worktreePath, baseRef)
}

// Remove removes a git worktree. repoPath is the main repository root;
// -C ensures git finds the repo regardless of the process working directory.
// --force is used intentionally: callers invoke Remove only when stopping or
// cleaning up a Worker (stop_worker, completed, split_request), at which point
// any uncommitted changes in the worktree are intentionally discarded.
func Remove(repoPath, worktreePath string) error {
	return RemoveContext(context.Background(), repoPath, worktreePath)
}

// RemoveContext removes a git worktree with cancellation support.
func RemoveContext(ctx context.Context, repoPath, worktreePath string) error {
	if !filepath.IsAbs(repoPath) {
		return fmt.Errorf("repoPath must be absolute, got: %s", repoPath)
	}
	if !filepath.IsAbs(worktreePath) {
		return fmt.Errorf("worktreePath must be absolute, got: %s", worktreePath)
	}
	return runContext(ctx, "git", "-C", repoPath, "worktree", "remove", "--force", worktreePath)
}

var ErrUnsafeBranchDelete = errors.New("unsafe branch delete skipped")
var ErrUnsafeBranchCreate = errors.New("unsafe branch create skipped")
var ErrOriginUnavailable = errors.New("origin unavailable")

// EnsureBranchCreationSafe verifies that creating branch would not collide with
// protected/default branches, checked-out branches, upstreams, or remote refs.
func EnsureBranchCreationSafe(repoPath, branch string) error {
	return EnsureBranchCreationSafeContext(context.Background(), repoPath, branch)
}

// EnsureBranchCreationSafeContext is EnsureBranchCreationSafe with cancellation support.
func EnsureBranchCreationSafeContext(ctx context.Context, repoPath, branch string) error {
	if !filepath.IsAbs(repoPath) {
		return fmt.Errorf("repoPath must be absolute, got: %s", repoPath)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("%w: branch must be non-empty", ErrUnsafeBranchCreate)
	}
	if exists, err := localBranchExists(ctx, repoPath, branch); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: branch %q already exists", ErrUnsafeBranchCreate, branch)
	}
	if reason, safe, err := branchSafety(ctx, repoPath, branch); err != nil {
		return err
	} else if !safe {
		return fmt.Errorf("%w: %s", ErrUnsafeBranchCreate, reason)
	}
	return nil
}

// DeleteTaskBranchIfSafe deletes a local branch only when it is clearly scoped
// to taskID and passes the normal branch deletion safety checks.
func DeleteTaskBranchIfSafe(repoPath, branch, taskID string) error {
	return DeleteTaskBranchIfSafeContext(context.Background(), repoPath, branch, taskID)
}

// DeleteTaskBranchIfSafeContext is DeleteTaskBranchIfSafe with cancellation support.
func DeleteTaskBranchIfSafeContext(ctx context.Context, repoPath, branch, taskID string) error {
	if !filepath.IsAbs(repoPath) {
		return fmt.Errorf("repoPath must be absolute, got: %s", repoPath)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil
	}
	containsTaskID := strings.Contains(branch, taskID)
	if containsTaskID && !BranchMatchesTaskID(branch, taskID) {
		return fmt.Errorf("%w: %q is not scoped to task %q", ErrUnsafeBranchDelete, branch, taskID)
	}
	if !containsTaskID && !isLegacyCleanupBranchName(branch) {
		return fmt.Errorf("%w: %q is not scoped to task %q", ErrUnsafeBranchDelete, branch, taskID)
	}
	if exists, err := localBranchExists(ctx, repoPath, branch); err != nil {
		return err
	} else if !exists {
		return nil
	}
	if reason, safe, err := branchSafety(ctx, repoPath, branch); err != nil {
		return err
	} else if !safe {
		return fmt.Errorf("%w: %s", ErrUnsafeBranchDelete, reason)
	}
	return runContext(ctx, "git", "-C", repoPath, "branch", "-D", branch)
}

func branchSafety(ctx context.Context, repoPath, branch string) (string, bool, error) {
	if isCommonDefaultBranchName(branch) {
		return fmt.Sprintf("%q is a protected default branch name", branch), false, nil
	}
	remotes, err := gitRemotes(ctx, repoPath)
	if err != nil {
		return "", false, err
	}
	defaultBranches, err := remoteDefaultBranchNames(ctx, repoPath, remotes)
	if err != nil {
		return "", false, err
	}
	for _, defaultBranch := range defaultBranches {
		if branch == defaultBranch {
			return fmt.Sprintf("%q is the remote default branch", branch), false, nil
		}
	}
	checkedOut, err := branchCheckedOutInWorktree(ctx, repoPath, branch)
	if err != nil {
		return "", false, err
	}
	if checkedOut {
		return fmt.Sprintf("%q is checked out in a worktree", branch), false, nil
	}
	hasRemote, err := branchHasRemoteRef(ctx, repoPath, branch)
	if err != nil {
		return "", false, err
	}
	if hasRemote {
		return fmt.Sprintf("%q has a remote ref and may have an open PR", branch), false, nil
	}
	existsOnRemote, err := branchExistsOnRemote(ctx, repoPath, remotes, branch)
	if err != nil {
		return "", false, err
	}
	if existsOnRemote {
		return fmt.Sprintf("%q exists on a remote and may have an open PR", branch), false, nil
	}
	hasUpstream, err := branchHasUpstream(ctx, repoPath, branch)
	if err != nil {
		return "", false, err
	}
	if hasUpstream {
		return fmt.Sprintf("%q has an upstream ref and may have an open PR", branch), false, nil
	}
	return "", true, nil
}

func localBranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	err := exec.CommandContext(ctx, "git", "-C", repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run()
	if err == nil {
		return true, nil
	}
	exitErr, ok := err.(*exec.ExitError)
	if ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check local branch %q: %w", branch, err)
}

func remoteDefaultBranchNames(ctx context.Context, repoPath string, remotes []string) ([]string, error) {
	var defaults []string
	seen := make(map[string]bool)
	for _, remote := range remotes {
		remoteDefaults, err := remoteDefaultBranchNamesForRemote(ctx, repoPath, remote)
		if err != nil {
			return nil, err
		}
		for _, defaultBranch := range remoteDefaults {
			if defaultBranch != "" && !seen[defaultBranch] {
				defaults = append(defaults, defaultBranch)
				seen[defaultBranch] = true
			}
		}
	}
	return defaults, nil
}

func gitRemotes(ctx context.Context, repoPath string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "remote").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list remotes: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.Fields(string(out)), nil
}

func remoteDefaultBranchNamesForRemote(ctx context.Context, repoPath, remote string) ([]string, error) {
	var defaults []string
	if out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD").CombinedOutput(); err == nil {
		ref := strings.TrimSpace(string(out))
		if i := strings.IndexByte(ref, '/'); i >= 0 {
			defaults = append(defaults, ref[i+1:])
		}
	}
	remoteCtx, cancel := contextWithDefaultTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(remoteCtx, "git", "-C", repoPath, "ls-remote", "--symref", remote, "HEAD").CombinedOutput()
	if err != nil {
		if len(defaults) > 0 {
			return defaults, nil
		}
		return nil, fmt.Errorf("%w: read remote default branch for %s: %w: %s", ErrOriginUnavailable, remote, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
			defaults = append(defaults, strings.TrimPrefix(fields[1], "refs/heads/"))
		}
	}
	return defaults, nil
}

func branchCheckedOutInWorktree(ctx context.Context, repoPath, branch string) (bool, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("list worktrees: %w: %s", err, strings.TrimSpace(string(out)))
	}
	want := "branch refs/heads/" + branch
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == want {
			return true, nil
		}
	}
	return false, nil
}

func branchHasRemoteRef(ctx context.Context, repoPath, branch string) (bool, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "for-each-ref", "--format=%(refname:short)", "refs/remotes").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("list remote branches: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || strings.HasSuffix(ref, "/HEAD") {
			continue
		}
		if remoteBranchName(ref) == branch {
			return true, nil
		}
	}
	return false, nil
}

func remoteBranchName(ref string) string {
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func branchExistsOnRemote(ctx context.Context, repoPath string, remotes []string, branch string) (bool, error) {
	if len(remotes) == 0 {
		return false, nil
	}
	want := "refs/heads/" + branch
	for _, remote := range remotes {
		remoteCtx, cancel := contextWithDefaultTimeout(ctx, 5*time.Second)
		out, err := exec.CommandContext(remoteCtx, "git", "-C", repoPath, "ls-remote", "--heads", remote, want).CombinedOutput()
		cancel()
		if err != nil {
			return false, fmt.Errorf("%w: check remote branch on %s: %w: %s", ErrOriginUnavailable, remote, err, strings.TrimSpace(string(out)))
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == want {
				return true, nil
			}
		}
	}
	return false, nil
}

func contextWithDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func branchHasUpstream(ctx context.Context, repoPath, branch string) (bool, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "for-each-ref", "--format=%(upstream:short)", "refs/heads/"+branch).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("read branch upstream: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func isCommonDefaultBranchName(branch string) bool {
	switch branch {
	case "main", "master", "trunk":
		return true
	default:
		return false
	}
}

func BranchMatchesTaskID(branch, taskID string) bool {
	if taskID == "" {
		return false
	}
	idx := strings.Index(branch, taskID)
	for idx >= 0 {
		end := idx + len(taskID)
		prevOK := idx == 0 || !isAlphaNumeric(branch[idx-1])
		nextOK := end == len(branch) || !isAlphaNumeric(branch[end])
		if prevOK && nextOK {
			return true
		}
		next := strings.Index(branch[idx+1:], taskID)
		if next < 0 {
			break
		}
		idx += next + 1
	}
	return false
}

func isAlphaNumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// isLegacyCleanupBranchName allows namespaced branches from workers spawned
// before branch names were required to embed task IDs. Callers pass the task's
// own ledger branch; this helper only rejects flat default-like names.
func isLegacyCleanupBranchName(branch string) bool {
	return strings.Contains(branch, "/")
}

func run(name string, args ...string) error {
	return runContext(context.Background(), name, args...)
}

func runContext(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}
