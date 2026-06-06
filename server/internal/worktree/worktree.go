package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Create creates a new git worktree at worktreePath branching from baseRef.
// repoPath is the main repository root. Fails if worktreePath already exists.
func Create(repoPath, branch, worktreePath, baseRef string) error {
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
	return run("git", "-C", repoPath, "worktree", "remove", "--force", worktreePath)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}
