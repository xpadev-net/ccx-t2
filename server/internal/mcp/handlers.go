package mcp

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/xpadev/ccx-t2/internal/config"
	githubpkg "github.com/xpadev/ccx-t2/internal/github"
	"github.com/xpadev/ccx-t2/internal/harness"
	"github.com/xpadev/ccx-t2/internal/ledger"
	"github.com/xpadev/ccx-t2/internal/tmux"
	"github.com/xpadev/ccx-t2/internal/worker"
	"github.com/xpadev/ccx-t2/internal/worktree"
)

// Deps holds the dependencies used by MCP tool handlers.
type Deps struct {
	Ledger   *ledger.Ledger
	Registry *worker.Registry
	Config   *config.Config
	GitHub   *githubpkg.Client // may be nil if GitHub is not configured
	Session  string            // tmux session name
	BaseURL  string            // e.g. "http://localhost:8080"
}

// RegisterOrchestratorTools registers the 10 Orchestrator tools on s.
func RegisterOrchestratorTools(s *Server, deps *Deps) {
	s.Register(ToolDef{
		Name:        "list_tasks",
		Description: "Return the current task ledger snapshot.",
		InputSchema: inSchema(map[string]any{}, nil),
	}, handleListTasks(deps))

	s.Register(ToolDef{
		Name:        "create_task",
		Description: "Add an unstarted task to the ledger.",
		InputSchema: inSchema(map[string]any{
			"title":           prop("string", "Task title"),
			"description":     prop("string", "Task description (body text)"),
			"allowed_files":   arrayProp("string", "Paths worker may edit"),
			"forbidden_files": arrayProp("string", "Paths worker must not edit"),
		}, []string{"title"}),
	}, handleCreateTask(deps))

	s.Register(ToolDef{
		Name:        "update_task",
		Description: "Update allowed fields of an existing task. Status cannot be changed here.",
		InputSchema: inSchema(map[string]any{
			"id":              prop("string", "Task ID"),
			"title":           prop("string", "New title"),
			"description":     prop("string", "New body text"),
			"allowed_files":   arrayProp("string", "New allowed_files list"),
			"forbidden_files": arrayProp("string", "New forbidden_files list"),
			"reason":          prop("string", "Reason note"),
		}, []string{"id"}),
	}, handleUpdateTask(deps))

	s.Register(ToolDef{
		Name:        "split_task",
		Description: "Split a task into sub-tasks.",
		InputSchema: inSchema(map[string]any{
			"id":     prop("string", "Task ID"),
			"reason": prop("string", "Reason for split"),
			"slices": map[string]any{
				"type":        "array",
				"description": "Child task definitions",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title":           prop("string", "Child title"),
						"description":     prop("string", "Child description"),
						"allowed_files":   arrayProp("string", "Allowed paths"),
						"forbidden_files": arrayProp("string", "Forbidden paths"),
					},
					"required": []string{"title"},
				},
			},
		}, []string{"id", "reason", "slices"}),
	}, handleSplitTask(deps))

	s.Register(ToolDef{
		Name:        "archive_task",
		Description: "Archive a completed task.",
		InputSchema: inSchema(map[string]any{
			"id": prop("string", "Task ID"),
		}, []string{"id"}),
	}, handleArchiveTask(deps))

	s.Register(ToolDef{
		Name:        "list_harnesses",
		Description: "List available worker harnesses and their usage.",
		InputSchema: inSchema(map[string]any{}, nil),
	}, handleListHarnesses(deps))

	s.Register(ToolDef{
		Name:        "spawn_worker",
		Description: "Create a worktree and tmux window, then start a worker harness.",
		InputSchema: inSchema(map[string]any{
			"task_id":         prop("string", "Task ID"),
			"branch":          prop("string", "Git branch name"),
			"allowed_files":   arrayProp("string", "Paths the worker may edit (required, >= 1)"),
			"forbidden_files": arrayProp("string", "Paths the worker must not edit"),
			"harness":         prop("string", "Harness name (optional if only one worker_harness)"),
		}, []string{"task_id", "branch", "allowed_files"}),
	}, handleSpawnWorker(deps))

	s.Register(ToolDef{
		Name:        "stop_worker",
		Description: "Kill a worker window, remove its worktree, and reset the task to unstarted.",
		InputSchema: inSchema(map[string]any{
			"worker_id": prop("string", "Worker ID (tmux window name)"),
		}, []string{"worker_id"}),
	}, handleStopWorker(deps))

	s.Register(ToolDef{
		Name:        "followup_worker",
		Description: "Send a follow-up message to a worker via tmux send-keys.",
		InputSchema: inSchema(map[string]any{
			"worker_id": prop("string", "Worker ID"),
			"message":   prop("string", "Message text"),
		}, []string{"worker_id", "message"}),
	}, handleFollowupWorker(deps))

	s.Register(ToolDef{
		Name:        "get_pr_status",
		Description: "Return the current GitHub PR state and CI checks.",
		InputSchema: inSchema(map[string]any{
			"pr_number": prop("integer", "Pull request number"),
		}, []string{"pr_number"}),
	}, handleGetPRStatus(deps))
}

// RegisterWorkerTools registers the notify tool on s.
func RegisterWorkerTools(s *Server, deps *Deps) {
	s.Register(ToolDef{
		Name:        "notify",
		Description: "Notify the server of a task completion, block, or split request.",
		InputSchema: inSchema(map[string]any{
			"type": prop("string", "completed | blocked | split_request"),
			"payload": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id":         prop("string", "Task ID"),
					"worker_id":       prop("string", "Caller's worker_id for ownership verification (optional but recommended)"),
					"pr_url":          prop("string", "PR URL (completed)"),
					"merge_commit":    prop("string", "Merge commit hash (completed)"),
					"reason":          prop("string", "Reason (blocked/split_request)"),
					"proposed_slices": arrayProp("object", "Child task specs (split_request)"),
				},
				"required": []string{"task_id"},
			},
		}, []string{"type", "payload"}),
	}, handleNotify(deps))
}

// ---- handler implementations ----

func handleListTasks(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		tasks, err := deps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		return tasks, nil
	}
}

func handleCreateTask(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		title, err := stringArg(args, "title")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(title) == "" {
			return nil, fmt.Errorf("title must be a non-empty string")
		}
		description := optionalStringArg(args, "description")
		allowedFiles, _ := stringSliceArg(args, "allowed_files")
		forbiddenFiles, _ := stringSliceArg(args, "forbidden_files")

		if err := ledger.ValidatePaths(allowedFiles); err != nil {
			return nil, fmt.Errorf("allowed_files: %w", err)
		}
		if err := ledger.ValidatePaths(forbiddenFiles); err != nil {
			return nil, fmt.Errorf("forbidden_files: %w", err)
		}

		// Use AddNew to generate ID and append atomically, avoiding a TOCTOU race
		// where concurrent create_task calls generate the same sequence number.
		t := ledger.Task{
			Title:          title,
			Status:         "unstarted",
			AllowedFiles:   allowedFiles,
			ForbiddenFiles: forbiddenFiles,
			Body:           description,
		}
		id, err := deps.Ledger.AddNew(t)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": id}, nil
	}
}

func handleUpdateTask(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		id, err := stringArg(args, "id")
		if err != nil {
			return nil, err
		}

		fields := make(map[string]any)
		for _, key := range []string{"title", "reason"} {
			if v, ok := args[key]; ok {
				if key == "title" {
					s, ok := v.(string)
					if !ok || strings.TrimSpace(s) == "" {
						return nil, fmt.Errorf("title must be a non-empty string")
					}
				}
				fields[key] = v
			}
		}
		if v, ok := args["description"]; ok {
			fields["body"] = v
		}
		for _, key := range []string{"allowed_files", "forbidden_files"} {
			if _, ok := args[key]; ok {
				sl, err := stringSliceArg(args, key)
				if err != nil {
					return nil, err
				}
				if err := ledger.ValidatePaths(sl); err != nil {
					return nil, fmt.Errorf("%s: %w", key, err)
				}
				fields[key] = sl
			}
		}
		// status is not updatable via update_task.
		if _, ok := args["status"]; ok {
			return nil, fmt.Errorf("status cannot be changed via update_task")
		}

		// Atomically reject updates to terminal tasks (completed, split) using
		// UpdateIfStatuses so the status check and write are in the same mutex lock,
		// preventing a concurrent transition from sneaking in between Load and Update.
		return nil, deps.Ledger.UpdateIfStatuses(id,
			[]string{"unstarted", "in_progress", "blocked"},
			fields)
	}
}

func handleSplitTask(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		id, err := stringArg(args, "id")
		if err != nil {
			return nil, err
		}
		reason := optionalStringArg(args, "reason")

		// Parse slices.
		slicesRaw, ok := args["slices"]
		if !ok {
			return nil, fmt.Errorf("slices is required")
		}
		slicesAny, ok := slicesRaw.([]any)
		if !ok || len(slicesAny) == 0 {
			return nil, fmt.Errorf("slices must be a non-empty array")
		}

		tasks, err := deps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		var original *ledger.Task
		for i := range tasks {
			if tasks[i].ID == id {
				original = &tasks[i]
				break
			}
		}
		if original == nil {
			return nil, fmt.Errorf("task not found: %s", id)
		}
		switch original.Status {
		case "split", "completed":
			return nil, fmt.Errorf("cannot split task with status %q", original.Status)
		}

		// Build all child tasks first (before any mutations) to validate paths.
		type childTask struct {
			id             string
			title          string
			desc           string
			allowedFiles   []string
			forbiddenFiles []string
		}
		// Validate all slices before allocating IDs.
		type pendingSlice struct {
			title, desc        string
			allowed, forbidden []string
		}
		pending := make([]pendingSlice, 0, len(slicesAny))
		for _, s := range slicesAny {
			sliceMap, ok := s.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("each slice must be an object")
			}
			childAllowed, _ := stringSliceArg(sliceMap, "allowed_files")
			childForbidden, _ := stringSliceArg(sliceMap, "forbidden_files")
			if err := ledger.ValidatePaths(childAllowed); err != nil {
				return nil, fmt.Errorf("slice allowed_files: %w", err)
			}
			if err := ledger.ValidatePaths(childForbidden); err != nil {
				return nil, fmt.Errorf("slice forbidden_files: %w", err)
			}
			childTitle, _ := sliceMap["title"].(string)
			if childTitle == "" {
				return nil, fmt.Errorf("slice %d: title is required", len(pending))
			}
			childDesc, _ := sliceMap["description"].(string)
			pending = append(pending, pendingSlice{
				title:   childTitle,
				desc:    childDesc,
				allowed: childAllowed, forbidden: childForbidden,
			})
		}
		// Generate all IDs in one read pass — loop GenerateID would return
		// duplicate IDs because the IDs are not on-disk until AddAll.
		childIDs, err := deps.Ledger.GenerateIDs(len(pending))
		if err != nil {
			return nil, err
		}
		children := make([]childTask, len(pending))
		for i, p := range pending {
			children[i] = childTask{
				id:             childIDs[i],
				title:          p.title,
				desc:           p.desc,
				allowedFiles:   p.allowed,
				forbiddenFiles: p.forbidden,
			}
		}

		// Atomically add all children first — if AddAll fails, the parent's
		// status is unchanged and the worker (if any) is still running, so
		// the caller can retry split_task without manual recovery.
		childTasks := make([]ledger.Task, len(children))
		for i, c := range children {
			childTasks[i] = ledger.Task{
				ID:             c.id,
				Title:          c.title,
				Status:         "unstarted",
				AllowedFiles:   c.allowedFiles,
				ForbiddenFiles: c.forbiddenFiles,
				Body:           c.desc,
			}
		}
		if err := deps.Ledger.AddAll(childTasks); err != nil {
			return nil, err
		}

		// Update parent to split only after all children are safely written.
		// Use UpdateReturnPrev to capture the actual pre-update task snapshot
		// atomically: a concurrent spawn_worker could have changed status from
		// "unstarted" to "in_progress" between our preflight read and this write.
		prevTask, err := deps.Ledger.UpdateReturnPrev(id, map[string]any{
			"status":    "split",
			"reason":    reason,
			"worker_id": "",
			"branch":    "",
			"harness":   "",
		})
		if err != nil {
			// Roll back the children to avoid orphans on retry.
			_ = deps.Ledger.DeleteTasks(childIDs)
			return nil, err
		}

		// Guard against concurrent transitions winning the race between our preflight
		// and the UpdateReturnPrev write.
		switch prevTask.Status {
		case "completed", "split":
			// Always a race — task reached a terminal state before our write.
			_ = deps.Ledger.DeleteTasks(childIDs)
			return nil, fmt.Errorf("cannot split task %s: it reached status %q concurrently", id, prevTask.Status)
		case "unstarted":
			// "unstarted" is expected when the original status was already "unstarted".
			// It's a race only when a concurrent stop_worker reset the task from
			// in_progress/blocked to "unstarted" before our UpdateReturnPrev.
			if original.Status != "unstarted" {
				_ = deps.Ledger.DeleteTasks(childIDs)
				return nil, fmt.Errorf("cannot split task %s: it reached status %q concurrently", id, prevTask.Status)
			}
		}

		// Clean up worker resources based on the actual previous status and
		// worker_id/branch from the atomic snapshot, not the stale preflight read.
		if prevTask.Status == "in_progress" || prevTask.Status == "blocked" {
			stopWorkerCleanup(deps, prevTask.WorkerID, prevTask.Branch, id)
		}

		return map[string]any{"child_ids": childIDs}, nil
	}
}

func handleArchiveTask(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		id, err := stringArg(args, "id")
		if err != nil {
			return nil, err
		}

		tasks, err := deps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		var t *ledger.Task
		for i := range tasks {
			if tasks[i].ID == id {
				t = &tasks[i]
				break
			}
		}
		if t == nil {
			return nil, fmt.Errorf("task not found: %s", id)
		}
		if t.Status != "completed" {
			return nil, fmt.Errorf("task %s is not completed (status: %s); only completed tasks can be archived", id, t.Status)
		}

		// Extract merge_commit from body comment.
		mergeCommit := extractMergeCommit(t.Body)

		if err := deps.Ledger.Archive(id, mergeCommit); err != nil {
			return nil, err
		}

		// Best-effort cleanup: remove worktree (may still exist if the server
		// crashed between notify(completed) and the worktree.Remove call) and
		// delete the git branch.
		wPath := filepath.Join(deps.Config.Project.WorktreeBase,
			deps.Config.Project.Slug+"-"+id)
		_ = worktree.Remove(deps.Config.Project.RepoPath, wPath)
		if t.Branch != "" {
			_ = exec.Command("git", "-C", deps.Config.Project.RepoPath, "branch", "-D", t.Branch).Run()
		}

		return map[string]any{"archived": id}, nil
	}
}

func handleListHarnesses(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		return harness.List(deps.Config), nil
	}
}

func handleSpawnWorker(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		taskID, err := stringArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		branch, err := stringArg(args, "branch")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(branch) == "" {
			return nil, fmt.Errorf("branch must be a non-empty string")
		}
		allowedFiles, err := stringSliceArg(args, "allowed_files")
		if err != nil {
			return nil, err
		}
		if len(allowedFiles) == 0 {
			return nil, fmt.Errorf("allowed_files must have at least one entry")
		}
		forbiddenFiles, _ := stringSliceArg(args, "forbidden_files")
		harnessName := optionalStringArg(args, "harness")

		// Preflight checks.
		tasks, err := deps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		var task *ledger.Task
		for i := range tasks {
			if tasks[i].ID == taskID {
				task = &tasks[i]
				break
			}
		}
		if task == nil {
			return nil, fmt.Errorf("task not found: %s", taskID)
		}
		if task.Status != "unstarted" {
			return nil, fmt.Errorf("task %s is not unstarted (status: %s)", taskID, task.Status)
		}

		// Validate paths.
		if err := ledger.ValidatePaths(allowedFiles); err != nil {
			return nil, fmt.Errorf("allowed_files: %w", err)
		}
		if err := ledger.ValidatePaths(forbiddenFiles); err != nil {
			return nil, fmt.Errorf("forbidden_files: %w", err)
		}
		if err := validateGitBranchName(branch); err != nil {
			return nil, err
		}

		// Resolve harness.
		resolvedHarness, hCfg, err := harness.Resolve(deps.Config, harnessName)
		if err != nil {
			return nil, err
		}

		// Validate mcp_args shell syntax and split into tokens before any state mutation.
		// Split the *template* before expanding {url}/{secret} so that the expanded
		// values (which may contain spaces or other shell metacharacters) are treated
		// as atomic tokens. Each token then has its placeholders substituted individually.
		mcpTokens, err := buildMCPTokens(hCfg.McpArgs, deps.BaseURL+"/mcp/worker", deps.Config.Server.McpSecret)
		if err != nil {
			return nil, fmt.Errorf("invalid mcp_args shell syntax: %w", err)
		}

		branchExists, err := gitBranchExists(deps.Config.Project.RepoPath, branch)
		if err != nil {
			return nil, err
		}
		if branchExists {
			return nil, fmt.Errorf("branch %q already exists", branch)
		}

		// Build paths.
		repoPath := deps.Config.Project.RepoPath
		worktreePath := filepath.Join(deps.Config.Project.WorktreeBase,
			deps.Config.Project.Slug+"-"+taskID)

		// Get current HEAD as base ref.
		headOut, err := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD").Output()
		if err != nil {
			return nil, fmt.Errorf("rev-parse HEAD: %w", err)
		}
		baseRef := strings.TrimSpace(string(headOut))

		// Step 1: Create worktree.
		if err := worktree.Create(repoPath, branch, worktreePath, baseRef); err != nil {
			return nil, fmt.Errorf("create worktree: %w", err)
		}

		workerID := "worker-" + taskID

		// Step 2: Create tmux window.
		if err := tmux.CreateWindow(deps.Session, workerID, worktreePath); err != nil {
			_ = worktree.Remove(repoPath, worktreePath)
			_ = exec.Command("git", "-C", repoPath, "branch", "-D", branch).Run()
			return nil, fmt.Errorf("create tmux window: %w", err)
		}

		// Step 3: Update ledger — use UpdateIfStatus to reject concurrent
		// modifications (e.g., a split_task that changed the status to "split"
		// between our preflight check and this write).
		updateErr := deps.Ledger.UpdateIfStatus(taskID, "unstarted", map[string]any{
			"status":          "in_progress",
			"branch":          branch,
			"worker_id":       workerID,
			"harness":         resolvedHarness,
			"allowed_files":   allowedFiles,
			"forbidden_files": forbiddenFiles,
		})
		if updateErr != nil {
			_ = tmux.KillWindow(deps.Session, workerID)
			_ = worktree.Remove(repoPath, worktreePath)
			_ = exec.Command("git", "-C", repoPath, "branch", "-D", branch).Run()
			return nil, fmt.Errorf("update ledger: %w", updateErr)
		}
		promptTask, err := loadTaskByID(deps.Ledger, taskID)
		if err != nil {
			rollbackSpawnAfterLedgerUpdate(deps, workerID, branch, taskID, repoPath, worktreePath)
			return nil, fmt.Errorf("reload task after update: %w", err)
		}

		// Register worker.
		deps.Registry.Register(worker.Info{
			TaskID:    taskID,
			WorkerID:  workerID,
			Harness:   resolvedHarness,
			StartedAt: time.Now(),
		})

		// Step 4: Launch harness with MCP args.
		// Rebuild the command by single-quoting each split token so that expanded
		// URL/secret values with shell metacharacters are not interpreted by the shell.
		harnessCmd := buildHarnessCommand(hCfg.Command, mcpTokens)
		if err := tmux.SendKeys(deps.Session, workerID, harnessCmd); err != nil {
			rollbackSpawnAfterLedgerUpdate(deps, workerID, branch, taskID, repoPath, worktreePath)
			return nil, fmt.Errorf("send harness command: %w", err)
		}

		// Step 5: Send task prompt (best-effort — worker is already running).
		// If this fails, include prompt_sent:false in the response so the orchestrator
		// knows to call followup_worker to deliver the task description.
		prompt := buildWorkerPromptFromTask(promptTask, taskID, workerID, branch,
			worktreePath, deps.Config.Project.ValidationCommand)
		promptSent := true
		if err := tmux.SendKeys(deps.Session, workerID, prompt); err != nil {
			log.Printf("warn: send task prompt: %v", err)
			promptSent = false
		}

		return map[string]any{
			"worker_id":     workerID,
			"worktree_path": worktreePath,
			"prompt_sent":   promptSent,
		}, nil
	}
}

func handleStopWorker(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		workerID, err := stringArg(args, "worker_id")
		if err != nil {
			return nil, err
		}

		// Find task in registry or ledger.
		// Always load branch from the ledger: the registry does not store it.
		var taskID, branch string
		if info, ok := deps.Registry.Get(workerID); ok {
			taskID = info.TaskID
		}
		// Ledger search fills branch (and taskID as fallback when not in registry).
		tasks, err := deps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			if t.WorkerID == workerID {
				if taskID == "" {
					taskID = t.ID
				}
				branch = t.Branch
				break
			}
		}

		// If neither the registry nor the ledger knows this worker, return an
		// error so the caller knows the ID was wrong. Best-effort kill the tmux
		// window in case it exists as an orphan.
		if taskID == "" {
			_ = tmux.KillWindow(deps.Session, workerID)
			return nil, fmt.Errorf("worker %q not found in registry or ledger", workerID)
		}

		// Commit ledger first, only when the task is in a recoverable state.
		// Reject if the task is already terminal to prevent overwriting
		// "completed" or "split" tasks back to "unstarted" in a narrow race
		// with notify(completed) or split_request cleanup.
		if err := deps.Ledger.UpdateIfStatuses(taskID,
			[]string{"in_progress", "blocked", "unstarted"},
			map[string]any{
				"status":    "unstarted",
				"worker_id": "",
				"branch":    "",
				"harness":   "",
				"reason":    "",
			}); err != nil {
			return nil, err
		}
		stopWorkerCleanup(deps, workerID, branch, taskID)

		return map[string]any{"stopped": workerID}, nil
	}
}

func handleFollowupWorker(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		workerID, err := stringArg(args, "worker_id")
		if err != nil {
			return nil, err
		}
		message, err := stringArg(args, "message")
		if err != nil {
			return nil, err
		}

		// Validate task status.
		tasks, err := deps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		var found bool
		for _, t := range tasks {
			if t.WorkerID == workerID {
				if t.Status != "in_progress" && t.Status != "blocked" {
					return nil, fmt.Errorf("task %s status is %q, cannot followup", t.ID, t.Status)
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("no task found with worker_id %q", workerID)
		}

		// Verify window exists.
		windowName := workerID
		alive, err := tmux.IsWindowAlive(deps.Session, windowName)
		if err != nil {
			return nil, fmt.Errorf("check window: %w", err)
		}
		if !alive {
			return nil, fmt.Errorf("tmux window %q does not exist", windowName)
		}

		if err := tmux.SendKeys(deps.Session, windowName, message); err != nil {
			return nil, fmt.Errorf("send keys: %w", err)
		}
		return map[string]any{"sent": true}, nil
	}
}

func handleGetPRStatus(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		prNumber, err := intArg(args, "pr_number")
		if err != nil {
			return nil, err
		}

		if prNumber <= 0 {
			return nil, fmt.Errorf("pr_number must be a positive integer")
		}
		if deps.GitHub == nil {
			return nil, fmt.Errorf("github.token, github.owner, and github.repo must be configured")
		}
		return deps.GitHub.GetPRStatus(ctx, prNumber)
	}
}

func handleNotify(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		notifyType, err := stringArg(args, "type")
		if err != nil {
			return nil, err
		}

		payloadRaw, ok := args["payload"]
		if !ok {
			return nil, fmt.Errorf("payload is required")
		}
		payload, ok := payloadRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("payload must be an object")
		}

		taskID, err := stringArg(payload, "task_id")
		if err != nil {
			return nil, err
		}

		// Optional worker_id field: if provided, verify the caller owns the task.
		// All workers share the same bearer token, so this provides a best-effort
		// ownership check preventing a buggy worker from notifying sibling tasks.
		callerWorkerID := optionalStringArg(payload, "worker_id")

		tasks, err := deps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		var task *ledger.Task
		for i := range tasks {
			if tasks[i].ID == taskID {
				task = &tasks[i]
				break
			}
		}
		if task == nil {
			return nil, fmt.Errorf("task not found: %s", taskID)
		}
		if callerWorkerID != "" && task.WorkerID != callerWorkerID {
			return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
				callerWorkerID, taskID, task.WorkerID)
		}
		// Validate the notify type before the status early-return so an unknown
		// type always returns an error, even for non-active tasks.
		switch notifyType {
		case "completed", "blocked", "split_request":
		default:
			return nil, fmt.Errorf("unknown notify type: %s", notifyType)
		}

		if task.Status != "in_progress" && task.Status != "blocked" {
			// Silently ignore notifications for non-active tasks.
			log.Printf("warn: notify %s for task %s with status %q — ignored",
				notifyType, taskID, task.Status)
			return map[string]any{"ignored": true}, nil
		}

		switch notifyType {
		case "completed":
			prURL, _ := payload["pr_url"].(string)
			mergeCommit, _ := payload["merge_commit"].(string)

			fields := map[string]any{
				"status":    "completed",
				"pr_url":    prURL,
				"worker_id": "",
				"harness":   "",
			}
			if mergeCommit != "" {
				// Append merge_commit comment to body so archive_task can read it.
				// Avoid a leading newline when body is empty.
				comment := "<!-- merge_commit: " + mergeCommit + " -->"
				var body string
				if task.Body == "" {
					body = comment
				} else {
					body = task.Body + "\n" + comment
				}
				fields["body"] = body
			}
			if err := deps.Ledger.UpdateIfStatuses(taskID, []string{"in_progress", "blocked"}, fields); err != nil {
				return nil, err
			}

			// Cleanup worker resources. The git branch is intentionally NOT deleted
			// here; it is preserved so archive_task can record it in the archive
			// front matter and then delete it. If archive_task is never called the
			// branch will be orphaned, but that matches the spec's deferred-deletion
			// design for completed tasks.
			wid := task.WorkerID
			if wid == "" {
				wid = "worker-" + taskID // fallback
			}
			_ = tmux.KillWindow(deps.Session, wid)
			_ = worktree.Remove(deps.Config.Project.RepoPath,
				filepath.Join(deps.Config.Project.WorktreeBase,
					deps.Config.Project.Slug+"-"+taskID))
			deps.Registry.Remove(wid)

		case "blocked":
			reason, _ := payload["reason"].(string)
			fields := map[string]any{"status": "blocked"}
			if reason != "" {
				fields["reason"] = reason
			}
			if err := deps.Ledger.UpdateIfStatuses(taskID, []string{"in_progress", "blocked"}, fields); err != nil {
				return nil, err
			}

		case "split_request":
			reason, _ := payload["reason"].(string)
			proposedSlices, _ := payload["proposed_slices"].([]any)
			if len(proposedSlices) == 0 {
				return nil, fmt.Errorf("proposed_slices must not be empty for split_request")
			}

			// Validate all slices before allocating IDs.
			type srChild struct {
				id, title, desc    string
				allowed, forbidden []string
			}
			type pendingSR struct {
				title, desc        string
				allowed, forbidden []string
			}
			pendingSRs := make([]pendingSR, 0, len(proposedSlices))
			for i, s := range proposedSlices {
				sliceMap, ok := s.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("proposed_slices[%d] must be an object", i)
				}
				childAllowed, _ := stringSliceArg(sliceMap, "allowed_files")
				childForbidden, _ := stringSliceArg(sliceMap, "forbidden_files")
				if err := ledger.ValidatePaths(childAllowed); err != nil {
					return nil, fmt.Errorf("proposed_slices[%d] allowed_files: %w", i, err)
				}
				if err := ledger.ValidatePaths(childForbidden); err != nil {
					return nil, fmt.Errorf("proposed_slices[%d] forbidden_files: %w", i, err)
				}
				childTitle, _ := sliceMap["title"].(string)
				if childTitle == "" {
					return nil, fmt.Errorf("proposed_slices[%d]: title is required", i)
				}
				childDesc, _ := sliceMap["description"].(string)
				pendingSRs = append(pendingSRs, pendingSR{
					title: childTitle, desc: childDesc,
					allowed: childAllowed, forbidden: childForbidden,
				})
			}
			// Generate all IDs in one read pass (loop GenerateID returns duplicates
			// since IDs are not on-disk yet).
			srIDs, err := deps.Ledger.GenerateIDs(len(pendingSRs))
			if err != nil {
				return nil, err
			}
			srChildren := make([]srChild, len(pendingSRs))
			for i, p := range pendingSRs {
				srChildren[i] = srChild{
					id: srIDs[i], title: p.title, desc: p.desc,
					allowed: p.allowed, forbidden: p.forbidden,
				}
			}

			// Atomically add all children before marking parent split.
			// AddAll writes all tasks in a single file operation, preventing
			// partial orphans on failure — parent stays in_progress so retry works.
			childTasks := make([]ledger.Task, len(srChildren))
			for i, c := range srChildren {
				childTasks[i] = ledger.Task{
					ID:             c.id,
					Title:          c.title,
					Status:         "unstarted",
					AllowedFiles:   c.allowed,
					ForbiddenFiles: c.forbidden,
					Body:           c.desc,
				}
			}
			if err := deps.Ledger.AddAll(childTasks); err != nil {
				return nil, err
			}

			// Update parent to split after children are safely written.
			// Use UpdateReturnPrev to detect if a concurrent notify(completed)
			// won the race and already moved the task to a terminal state.
			srChildIDs := make([]string, len(srChildren))
			for i, c := range srChildren {
				srChildIDs[i] = c.id
			}
			prevTask, updateErr := deps.Ledger.UpdateReturnPrev(taskID, map[string]any{
				"status":    "split",
				"worker_id": "",
				"branch":    "",
				"harness":   "",
				"reason":    reason,
			})
			if updateErr != nil {
				_ = deps.Ledger.DeleteTasks(srChildIDs)
				return nil, updateErr
			}
			// Also guard "unstarted": a concurrent stop_worker may have reset the task
			// between the preflight check and UpdateReturnPrev, leaving prevTask.WorkerID
			// empty and cleanup a no-op while children are permanently orphaned.
			switch prevTask.Status {
			case "completed", "split", "unstarted":
				_ = deps.Ledger.DeleteTasks(srChildIDs)
				return nil, fmt.Errorf("cannot split task %s: it reached status %q concurrently", taskID, prevTask.Status)
			}

			// Cleanup resources using prevTask snapshot (not stale task load) so that
			// a concurrent stop_worker + spawn_worker that updated worker_id/branch
			// between the initial load and UpdateReturnPrev is handled correctly.
			wid := prevTask.WorkerID
			if wid == "" {
				wid = "worker-" + taskID
			}
			_ = tmux.KillWindow(deps.Session, wid)
			_ = worktree.Remove(deps.Config.Project.RepoPath,
				filepath.Join(deps.Config.Project.WorktreeBase,
					deps.Config.Project.Slug+"-"+taskID))
			if prevTask.Branch != "" {
				_ = exec.Command("git", "-C", deps.Config.Project.RepoPath,
					"branch", "-D", prevTask.Branch).Run()
			}
			deps.Registry.Remove(wid)

		}

		return map[string]any{"ok": true}, nil
	}
}

// ---- helpers ----

// stopWorkerCleanup is best-effort cleanup: it kills the tmux window,
// removes the worktree, deletes the git branch, and evicts the registry entry.
// All errors are silently ignored; callers must not rely on this returning nil.
func stopWorkerCleanup(deps *Deps, workerID, branch, taskID string) {
	if workerID != "" {
		_ = tmux.KillWindow(deps.Session, workerID)
	}
	if taskID != "" {
		wPath := filepath.Join(deps.Config.Project.WorktreeBase,
			deps.Config.Project.Slug+"-"+taskID)
		_ = worktree.Remove(deps.Config.Project.RepoPath, wPath)
	}
	if branch != "" {
		_ = exec.Command("git", "-C", deps.Config.Project.RepoPath,
			"branch", "-D", branch).Run()
	}
	if workerID != "" {
		deps.Registry.Remove(workerID)
	}
}

// extractMergeCommit reads the <!-- merge_commit: ... --> comment from a body.
func extractMergeCommit(body string) string {
	const prefix = "<!-- merge_commit: "
	const suffix = " -->"
	idx := strings.Index(body, prefix)
	if idx == -1 {
		return ""
	}
	rest := body[idx+len(prefix):]
	end := strings.Index(rest, suffix)
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func validateGitBranchName(branch string) error {
	out, err := exec.Command("git", "check-ref-format", "--branch", branch).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("invalid branch %q: %s", branch, msg)
	}
	return nil
}

func gitBranchExists(repoPath, branch string) (bool, error) {
	out, err := exec.Command("git", "-C", repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).CombinedOutput()
	if err == nil {
		return true, nil
	}
	exitErr, ok := err.(*exec.ExitError)
	if ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return false, fmt.Errorf("check branch existence: %s", msg)
}

func loadTaskByID(l *ledger.Ledger, taskID string) (*ledger.Task, error) {
	tasks, err := l.Load()
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].ID == taskID {
			return &tasks[i], nil
		}
	}
	return nil, fmt.Errorf("task not found: %s", taskID)
}

func rollbackSpawnAfterLedgerUpdate(deps *Deps, workerID, branch, taskID, repoPath, worktreePath string) {
	_ = tmux.KillWindow(deps.Session, workerID)
	_ = worktree.Remove(repoPath, worktreePath)
	_ = exec.Command("git", "-C", repoPath, "branch", "-D", branch).Run()
	// Reset lifecycle fields only — do not restore allowed/forbidden_files
	// to avoid overwriting concurrent update_task edits. Use UpdateIfStatuses
	// so a concurrent split_task that already committed (moving the parent to
	// "split") is not overwritten by this rollback.
	if rollbackErr := deps.Ledger.UpdateIfStatuses(taskID, []string{"in_progress"}, map[string]any{
		"status":    "unstarted",
		"worker_id": "",
		"branch":    "",
		"harness":   "",
	}); rollbackErr != nil {
		log.Printf("error: spawn_worker rollback failed for task %s: %v — task may be stuck in_progress", taskID, rollbackErr)
	}
	deps.Registry.Remove(workerID)
}

func buildMCPTokens(template, workerMCPURL, secret string) ([]string, error) {
	templateTokens, err := shellquote.Split(template)
	if err != nil {
		return nil, err
	}
	tokens := make([]string, len(templateTokens))
	for i, tok := range templateTokens {
		tokens[i] = replaceMcpURL(tok, workerMCPURL, secret)
	}
	return tokens, nil
}

func buildHarnessCommand(command string, mcpTokens []string) string {
	parts := make([]string, 0, 1+len(mcpTokens))
	parts = append(parts, shellQuoteArg(command))
	for _, tok := range mcpTokens {
		parts = append(parts, shellQuoteArg(tok))
	}
	return strings.Join(parts, " ")
}

func buildWorkerPromptFromTask(task *ledger.Task, taskID, workerID, branch, worktreePath, validationCmd string) string {
	return buildWorkerPrompt(task, taskID, workerID, branch, task.AllowedFiles, task.ForbiddenFiles,
		worktreePath, validationCmd)
}

// buildWorkerPrompt constructs the stdin prompt sent to the worker harness.
// workerID is included so the worker can pass it in notify payloads for
// ownership verification by the server.
func buildWorkerPrompt(task *ledger.Task, taskID, workerID, branch string, allowedFiles, forbiddenFiles []string,
	worktreePath, validationCmd string) string {
	var sb strings.Builder
	sb.WriteString("You are a Worker agent. Complete the following task:\n\n")
	sb.WriteString("Task ID: " + taskID + "\n")
	sb.WriteString("Worker ID: " + workerID + "\n")
	sb.WriteString("Title: " + task.Title + "\n")
	sb.WriteString("Branch: " + branch + "\n")
	sb.WriteString("Worktree: " + worktreePath + "\n")
	if task.Body != "" {
		sb.WriteString("\nDescription:\n" + task.Body + "\n")
	}
	sb.WriteString("\nAllowed files (directory-boundary prefix match):\n")
	for _, f := range allowedFiles {
		sb.WriteString("  - " + f + "\n")
	}
	if len(forbiddenFiles) > 0 {
		sb.WriteString("\nForbidden files (do NOT edit these):\n")
		for _, f := range forbiddenFiles {
			sb.WriteString("  - " + f + "\n")
		}
	}
	if validationCmd != "" {
		sb.WriteString("\nValidation command: " + validationCmd + "\n")
	}
	sb.WriteString(`
Instructions:
- Only edit files within the allowed_files paths (directory-boundary prefix match).
- Do not edit any forbidden_files.
- Implement the task, validate, self-review, create a PR, and merge it.
- Always include your worker_id in notify payloads for ownership verification.
- When complete: call notify(type="completed", payload={task_id, worker_id, pr_url, merge_commit}).
- If blocked: call notify(type="blocked", payload={task_id, worker_id, reason}).
- If you need to split: call notify(type="split_request", payload={task_id, worker_id, reason, proposed_slices}).
`)
	return sb.String()
}

// shellQuoteArg wraps a single argument in POSIX single quotes so that shell
// metacharacters in expanded values (URL, secret) are not interpreted.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
