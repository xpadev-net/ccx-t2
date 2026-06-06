package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Create creates a new git worktree at worktreePath branching from baseRef.
// It fails if worktreePath already exists.
func Create(repoPath, branch, worktreePath, baseRef string) error {
	if _, err := os.Stat(worktreePath); err == nil {
		return fmt.Errorf("worktree path already exists: %s", worktreePath)
	}
	return run("git", "-C", repoPath, "worktree", "add", "-b", branch, worktreePath, baseRef)
}

// Remove removes a git worktree by path. The --force flag is used to handle
// dirty or detached worktrees.
func Remove(worktreePath string) error {
	return run("git", "worktree", "remove", "--force", worktreePath)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}
