package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/xpadev/ccx-t2/internal/config"
	githubpkg "github.com/xpadev/ccx-t2/internal/github"
	"github.com/xpadev/ccx-t2/internal/harness"
	"github.com/xpadev/ccx-t2/internal/ledger"
	runtimepkg "github.com/xpadev/ccx-t2/internal/runtime"
	"github.com/xpadev/ccx-t2/internal/tmux"
	"github.com/xpadev/ccx-t2/internal/worker"
	"github.com/xpadev/ccx-t2/internal/worktree"
)

var (
	mergeCommitRE              = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	cleanupPendingCommentRE    = regexp.MustCompile(`(?m)\n?<!-- cleanup_pending: ([A-Za-z0-9+/=]+) -->`)
	cleanupPendingCommentStart = "<!-- cleanup_pending: "
	cleanupPendingCommentEnd   = " -->"
)

type notifyTriggerer interface {
	Trigger(ctx context.Context, reason string) error
}

// Deps holds the dependencies used by MCP tool handlers.
type Deps struct {
	Ledger      *ledger.Ledger
	Registry    *worker.Registry
	Config      *config.Config
	GitHub      *githubpkg.Client // may be nil if GitHub is not configured
	Session     string            // tmux session name
	BaseURL     string            // e.g. "http://localhost:8080"
	Manager     *runtimepkg.Manager
	ProjectSlug string

	NotifyTrigger notifyTriggerer

	notifyAfterOwnershipPreflight func()
	cleanup                       workerCleanupOps
	spawn                         spawnWorkerOps
}

type cleanupIntent string

const (
	cleanupIntentStopWorker   cleanupIntent = "stop_worker"
	cleanupIntentCompleted    cleanupIntent = "completed"
	cleanupIntentSplitRequest cleanupIntent = "split_request"

	cleanupOriginalSplitReasonPrefix = "\noriginal split reason: "

	spawnWorkerTimeout         = 2 * time.Minute
	spawnWorkerRollbackTimeout = 15 * time.Second
	cleanupTaskBranchTimeout   = 10 * time.Second
)

type workerCleanupOps struct {
	isWindowAlive    func(session, window string) (bool, error)
	killWindow       func(session, window string) error
	removeWorktree   func(repoPath, worktreePath string) error
	deleteTaskBranch func(repoPath, branch, taskID string) error
}

func (ops workerCleanupOps) withDefaults() workerCleanupOps {
	if ops.isWindowAlive == nil {
		ops.isWindowAlive = tmux.IsWindowAlive
	}
	if ops.killWindow == nil {
		ops.killWindow = tmux.KillWindow
	}
	if ops.removeWorktree == nil {
		ops.removeWorktree = worktree.Remove
	}
	if ops.deleteTaskBranch == nil {
		ops.deleteTaskBranch = cleanupTaskBranch
	}
	return ops
}

type spawnWorkerOps struct {
	validateBranchName       func(ctx context.Context, branch string) error
	ensureBranchCreationSafe func(ctx context.Context, repoPath, branch string) error
	headRef                  func(ctx context.Context, repoPath string) (string, error)
	createWorktree           func(ctx context.Context, repoPath, branch, worktreePath, baseRef string) error
	createWindow             func(ctx context.Context, session, name, startDir string) error
	killWindow               func(ctx context.Context, session, window string) error
	removeWorktree           func(ctx context.Context, repoPath, worktreePath string) error
	deleteTaskBranch         func(ctx context.Context, repoPath, branch, taskID string) error
	sendKeys                 func(ctx context.Context, session, window, keys string) error
	waitForHarnessProcess    func(ctx context.Context, session, window string, timeout time.Duration) error
}

func (ops spawnWorkerOps) withDefaults() spawnWorkerOps {
	if ops.validateBranchName == nil {
		ops.validateBranchName = validateGitBranchNameContext
	}
	if ops.ensureBranchCreationSafe == nil {
		ops.ensureBranchCreationSafe = worktree.EnsureBranchCreationSafeContext
	}
	if ops.headRef == nil {
		ops.headRef = worktree.HeadRefContext
	}
	if ops.createWorktree == nil {
		ops.createWorktree = worktree.CreateContext
	}
	if ops.createWindow == nil {
		ops.createWindow = tmux.CreateWindowContext
	}
	if ops.killWindow == nil {
		ops.killWindow = tmux.KillWindowContext
	}
	if ops.removeWorktree == nil {
		ops.removeWorktree = worktree.RemoveContext
	}
	if ops.deleteTaskBranch == nil {
		ops.deleteTaskBranch = cleanupTaskBranchContext
	}
	if ops.sendKeys == nil {
		ops.sendKeys = tmux.SendKeysContext
	}
	if ops.waitForHarnessProcess == nil {
		ops.waitForHarnessProcess = waitForHarnessProcessContext
	}
	return ops
}

type workerCleanupResult struct {
	WorkerID     string
	WorktreePath string
	Branch       string
	TmuxErr      error
	WorktreeErr  error
	BranchErr    error
}

func (r workerCleanupResult) Failed() bool {
	return r.TmuxErr != nil || r.WorktreeErr != nil || r.BranchErr != nil
}

func (r workerCleanupResult) Err() error {
	if !r.Failed() {
		return nil
	}
	return &workerCleanupError{result: r}
}

func (r workerCleanupResult) errors() []error {
	var errs []error
	if r.TmuxErr != nil {
		errs = append(errs, r.TmuxErr)
	}
	if r.WorktreeErr != nil {
		errs = append(errs, r.WorktreeErr)
	}
	if r.BranchErr != nil {
		errs = append(errs, r.BranchErr)
	}
	return errs
}

type workerCleanupError struct {
	result workerCleanupResult
}

type cleanupPendingMarker struct {
	Intent         string `json:"intent"`
	OriginalReason string `json:"original_reason,omitempty"`
}

func (e *workerCleanupError) Error() string {
	errs := e.result.errors()
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, err.Error())
	}
	return "worker cleanup failed: " + strings.Join(parts, "; ")
}

func (e *workerCleanupError) Unwrap() []error {
	return e.result.errors()
}

// RegisterOrchestratorTools registers the Orchestrator tools on s.
func RegisterOrchestratorTools(s *Server, deps *Deps) {
	s.Register(ToolDef{
		Name:        "list_projects",
		Description: "Return the configured project list.",
		InputSchema: inSchema(map[string]any{}, nil),
	}, handleListProjects(deps))

	s.Register(ToolDef{
		Name:        "list_tasks",
		Description: "Return the current task ledger snapshot.",
		InputSchema: inSchema(projectProps(nil), nil),
	}, handleListTasks(deps))

	s.Register(ToolDef{
		Name:        "create_task",
		Description: "Add an unstarted task to the ledger. For raw natural-language intake, pass request without title or allowed_files.",
		InputSchema: inSchema(projectProps(map[string]any{
			"title":           prop("string", "Task title"),
			"request":         prop("string", "Raw natural-language request to investigate"),
			"description":     prop("string", "Task description (body text)"),
			"allowed_files":   arrayProp("string", "Paths worker may edit"),
			"forbidden_files": arrayProp("string", "Paths worker must not edit"),
		}), nil),
	}, handleCreateTask(deps))

	s.Register(ToolDef{
		Name:        "update_task",
		Description: "Update allowed fields of an existing task. Status cannot be changed here.",
		InputSchema: inSchema(projectProps(map[string]any{
			"id":              prop("string", "Task ID"),
			"title":           prop("string", "New title"),
			"description":     prop("string", "New body text"),
			"allowed_files":   arrayProp("string", "New allowed_files list"),
			"forbidden_files": arrayProp("string", "New forbidden_files list"),
			"reason":          prop("string", "Reason note"),
		}), []string{"id"}),
	}, handleUpdateTask(deps))

	s.Register(ToolDef{
		Name:        "split_task",
		Description: "Split a task into sub-tasks.",
		InputSchema: inSchema(projectProps(map[string]any{
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
		}), []string{"id", "reason", "slices"}),
	}, handleSplitTask(deps))

	s.Register(ToolDef{
		Name:        "archive_task",
		Description: "Archive a completed task.",
		InputSchema: inSchema(projectProps(map[string]any{
			"id": prop("string", "Task ID"),
		}), []string{"id"}),
	}, handleArchiveTask(deps))

	s.Register(ToolDef{
		Name:        "list_harnesses",
		Description: "List available worker harnesses and their usage.",
		InputSchema: inSchema(map[string]any{}, nil),
	}, handleListHarnesses(deps))

	s.Register(ToolDef{
		Name:        "spawn_worker",
		Description: "Create a worktree and tmux window, then start a worker harness.",
		InputSchema: inSchema(projectProps(map[string]any{
			"task_id":         prop("string", "Task ID"),
			"branch":          prop("string", "Git branch name"),
			"allowed_files":   arrayProp("string", "Paths the worker may edit (required, >= 1)"),
			"forbidden_files": arrayProp("string", "Paths the worker must not edit"),
			"harness":         prop("string", "Harness name (optional if only one worker_harness)"),
		}), []string{"task_id", "branch", "allowed_files"}),
	}, handleSpawnWorker(deps))

	s.Register(ToolDef{
		Name:        "stop_worker",
		Description: "Kill a worker window, remove its worktree, and reset the task to unstarted or finish pending cleanup.",
		InputSchema: inSchema(projectProps(map[string]any{
			"worker_id": prop("string", "Worker ID (tmux window name)"),
		}), []string{"worker_id"}),
	}, handleStopWorker(deps))

	s.Register(ToolDef{
		Name:        "followup_worker",
		Description: "Send a follow-up message to a worker via tmux send-keys.",
		InputSchema: inSchema(projectProps(map[string]any{
			"worker_id": prop("string", "Worker ID"),
			"message":   prop("string", "Message text"),
		}), []string{"worker_id", "message"}),
	}, handleFollowupWorker(deps))

	s.Register(ToolDef{
		Name:        "get_pr_status",
		Description: "Return the current GitHub PR state and CI checks.",
		InputSchema: inSchema(projectProps(map[string]any{
			"pr_number": prop("integer", "Pull request number"),
		}), []string{"pr_number"}),
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
					"project_slug":    prop("string", "Project slug"),
					"task_id":         prop("string", "Task ID"),
					"worker_id":       prop("string", "Caller's worker_id for ownership verification"),
					"pr_url":          prop("string", "PR URL (completed)"),
					"merge_commit":    prop("string", "Merge commit hash (completed)"),
					"reason":          prop("string", "Reason (blocked/split_request)"),
					"proposed_slices": arrayProp("object", "Child task specs (split_request)"),
				},
				"required": []string{"task_id", "worker_id"},
			},
		}, []string{"type", "payload"}),
	}, handleNotify(deps))
}

// ---- handler implementations ----

func handleListTasks(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
		tasks, err := toolDeps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		return tasks, nil
	}
}

func handleListProjects(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		if deps.Manager != nil {
			return deps.Manager.Projects(), nil
		}
		if deps.Config == nil {
			return nil, fmt.Errorf("config is not configured")
		}
		return []runtimepkg.ProjectInfo{{
			Slug:     deps.Config.Project.Slug,
			RepoPath: deps.Config.Project.RepoPath,
		}}, nil
	}
}

func handleCreateTask(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
		title := ""
		if _, ok := args["title"]; ok {
			var err error
			title, err = stringArg(args, "title")
			if err != nil {
				return nil, err
			}
		}
		description := optionalStringArg(args, "description")
		if strings.TrimSpace(description) == "" {
			if request := optionalStringArg(args, "request"); strings.TrimSpace(request) != "" {
				description = request
			}
		}
		if strings.TrimSpace(title) == "" && strings.TrimSpace(description) == "" {
			return nil, fmt.Errorf("title, description, or request is required")
		}
		if strings.TrimSpace(title) == "" {
			title = "Natural language intake"
		}
		allowedFiles, err := fileListArg(args, "allowed_files")
		if err != nil {
			return nil, fmt.Errorf("allowed_files: %w", err)
		}
		forbiddenFiles, err := fileListArg(args, "forbidden_files")
		if err != nil {
			return nil, fmt.Errorf("forbidden_files: %w", err)
		}

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
		id, err := toolDeps.Ledger.AddNew(t)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": id}, nil
	}
}

func handleUpdateTask(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
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
				sl, err := fileListArg(args, key)
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

		// Atomically reject updates to terminal tasks (completed, split). When a
		// task is blocked on cleanup, preserve the internal retry marker even if
		// the visible description is edited via update_task.
		_, err = toolDeps.Ledger.UpdateIfStatusesReturnPrevWith(id,
			[]string{"unstarted", "in_progress", "blocked"},
			func(current ledger.Task) (map[string]any, error) {
				next := make(map[string]any, len(fields))
				for k, v := range fields {
					next[k] = v
				}
				if bodyValue, ok := next["body"]; ok {
					if body, ok := bodyValue.(string); ok {
						if pendingIntent, originalReason, ok := cleanupIntentFromTask(current); ok {
							next["body"] = bodyWithCleanupPending(body, pendingIntent, originalReason)
						}
					}
				}
				return next, nil
			})
		return nil, err
	}
}

func handleSplitTask(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
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

		tasks, err := toolDeps.Ledger.Load()
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
		if pendingIntent, originalReason, ok := cleanupIntentFromTask(*original); ok {
			if pendingIntent != cleanupIntentSplitRequest {
				return nil, fmt.Errorf("task %s has %s cleanup pending; retry that cleanup before split_task", id, pendingIntent)
			}
			pendingTask, err := toolDeps.Ledger.UpdateIfStatusesReturnPrevWith(id, []string{"blocked"},
				func(current ledger.Task) (map[string]any, error) {
					currentIntent, currentOriginalReason, ok := cleanupIntentFromTask(current)
					if !ok {
						return nil, fmt.Errorf("task %s is missing cleanup marker", id)
					}
					if currentIntent != cleanupIntentSplitRequest {
						return nil, fmt.Errorf("task %s has %s cleanup pending, not %s",
							id, currentIntent, cleanupIntentSplitRequest)
					}
					originalReason = currentOriginalReason
					return map[string]any{
						"reason": cleanupPendingReason(cleanupIntentSplitRequest, nil, originalReason),
					}, nil
				})
			if err != nil {
				return nil, err
			}
			wid := pendingTask.WorkerID
			if wid == "" {
				wid = workerIDFor(toolDeps, id)
			}
			result := cleanupWorkerResources(toolDeps, wid, pendingTask.Branch, id, true)
			if cleanupErr := result.Err(); cleanupErr != nil {
				if err := markCleanupPending(toolDeps, id, wid, pendingTask, cleanupIntentSplitRequest, originalReason, cleanupErr); err != nil {
					return nil, fmt.Errorf("%w; additionally failed to record cleanup state: %v", cleanupErr, err)
				}
				return nil, cleanupErr
			}
			if err := finishPendingCleanup(toolDeps, id, wid, cleanupIntentSplitRequest, originalReason); err != nil {
				return nil, err
			}
			return map[string]any{"child_ids": []string{}, "cleanup_retried": true}, nil
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
		for i, s := range slicesAny {
			sliceMap, ok := s.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("each slice must be an object")
			}
			childAllowed, err := fileListArg(sliceMap, "allowed_files")
			if err != nil {
				return nil, fmt.Errorf("slice %d allowed_files: %w", i, err)
			}
			childForbidden, err := fileListArg(sliceMap, "forbidden_files")
			if err != nil {
				return nil, fmt.Errorf("slice %d forbidden_files: %w", i, err)
			}
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
		childIDs, err := toolDeps.Ledger.GenerateIDs(len(pending))
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
		if err := toolDeps.Ledger.AddAll(childTasks); err != nil {
			return nil, err
		}

		// Update parent to split only after all children are safely written.
		// Keep the allowed status set tied to the preflight state so concurrent
		// terminal transitions or stop_worker resets are not overwritten.
		allowedStatuses := []string{original.Status}
		if original.Status == "in_progress" || original.Status == "blocked" {
			allowedStatuses = []string{"in_progress", "blocked"}
		}
		prevTask, err := toolDeps.Ledger.UpdateIfStatusesReturnPrevWith(id, allowedStatuses, func(current ledger.Task) (map[string]any, error) {
			if pendingIntent, _, ok := cleanupIntentFromTask(current); ok {
				return nil, fmt.Errorf("task %s has %s cleanup pending; retry that cleanup before split_task", id, pendingIntent)
			}
			if current.Status == "in_progress" || current.Status == "blocked" {
				return map[string]any{
					"status": "blocked",
					"reason": cleanupPendingReason(cleanupIntentSplitRequest, nil, reason),
					"body":   bodyWithCleanupPending(current.Body, cleanupIntentSplitRequest, reason),
				}, nil
			}
			return map[string]any{
				"status":    "split",
				"reason":    reason,
				"worker_id": "",
				"branch":    "",
				"harness":   "",
			}, nil
		})
		if err != nil {
			// Roll back the children to avoid orphans on retry.
			_ = toolDeps.Ledger.DeleteTasks(childIDs)
			return nil, err
		}

		// Clean up worker resources based on the actual previous status and
		// worker_id/branch from the atomic snapshot, not the stale preflight read.
		if prevTask.Status == "in_progress" || prevTask.Status == "blocked" {
			wid := prevTask.WorkerID
			if wid == "" {
				wid = workerIDFor(toolDeps, id)
			}
			result := cleanupWorkerResources(toolDeps, wid, prevTask.Branch, id, true)
			if cleanupErr := result.Err(); cleanupErr != nil {
				if err := markCleanupPending(toolDeps, id, wid, prevTask, cleanupIntentSplitRequest, reason, cleanupErr); err != nil {
					return nil, fmt.Errorf("%w; additionally failed to record cleanup state: %v", cleanupErr, err)
				}
				return nil, cleanupErr
			}
			if err := finishPendingCleanup(toolDeps, id, wid, cleanupIntentSplitRequest, reason); err != nil {
				return nil, err
			}
		}

		return map[string]any{"child_ids": childIDs}, nil
	}
}

func handleArchiveTask(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
		id, err := stringArg(args, "id")
		if err != nil {
			return nil, err
		}

		tasks, err := toolDeps.Ledger.Load()
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
			archivedTask, archived, err := toolDeps.Ledger.ArchivedTask(id)
			if err != nil {
				return nil, err
			}
			if archived {
				cleanupArchivedTaskResources(toolDeps, id, archivedTask.Branch)
				return map[string]any{"archived": id}, nil
			}
			return nil, fmt.Errorf("task not found: %s", id)
		}
		if t.Status != "completed" {
			return nil, fmt.Errorf("task %s is not completed (status: %s); only completed tasks can be archived", id, t.Status)
		}

		// Extract merge_commit from body comment.
		mergeCommit := extractMergeCommit(t.Body)

		if err := toolDeps.Ledger.Archive(id, mergeCommit); err != nil {
			return nil, err
		}

		cleanupArchivedTaskResources(toolDeps, id, t.Branch)

		return map[string]any{"archived": id}, nil
	}
}

func handleListHarnesses(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		return harness.List(deps.Config), nil
	}
}

func cleanupArchivedTaskResources(deps *Deps, taskID, branch string) {
	workerID := workerIDFor(deps, taskID)
	_ = tmux.KillWindow(deps.Session, workerID)
	deps.Registry.Remove(workerID)
	wPath, err := config.ProjectWorktreePath(deps.Config.Project, taskID)
	if err != nil {
		log.Printf("warn: archived task cleanup skipped unsafe worktree path for task %s: %v", taskID, err)
	} else {
		_ = worktree.Remove(deps.Config.Project.RepoPath, wPath)
	}
	if branch != "" {
		_ = cleanupTaskBranch(deps.Config.Project.RepoPath, branch, taskID)
	}
}

func ensureWorkerTaskActive(l *ledger.Ledger, workerID string) error {
	tasks, err := l.Load()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.WorkerID != workerID {
			continue
		}
		if t.Status != "in_progress" && t.Status != "blocked" {
			return fmt.Errorf("task %s status is %q, cannot followup", t.ID, t.Status)
		}
		return nil
	}
	return fmt.Errorf("no task found with worker_id %q", workerID)
}

func handleSpawnWorker(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
		ops := toolDeps.spawn.withDefaults()
		spawnCtx, cancel := context.WithTimeout(ctx, spawnWorkerTimeout)
		defer cancel()

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
		allowedFiles, err := fileListArg(args, "allowed_files")
		if err != nil {
			return nil, err
		}
		if len(allowedFiles) == 0 {
			return nil, fmt.Errorf("allowed_files must have at least one entry")
		}
		forbiddenFiles, err := fileListArg(args, "forbidden_files")
		if err != nil {
			return nil, fmt.Errorf("forbidden_files: %w", err)
		}
		harnessName := optionalStringArg(args, "harness")

		// Preflight checks.
		tasks, err := toolDeps.Ledger.Load()
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
		if err := ops.validateBranchName(spawnCtx, branch); err != nil {
			return nil, err
		}
		if !worktree.BranchMatchesTaskID(branch, taskID) {
			return nil, fmt.Errorf("branch %q must include task_id %q as a path or delimiter-bounded segment", branch, taskID)
		}

		// Resolve harness.
		resolvedHarness, hCfg, err := harness.Resolve(toolDeps.Config, harnessName)
		if err != nil {
			return nil, err
		}

		// Validate mcp_args shell syntax and split into tokens before any state mutation.
		// Split the *template* before expanding {url}/{secret} so that the expanded
		// values (which may contain spaces or other shell metacharacters) are treated
		// as atomic tokens. Each token then has its placeholders substituted individually.
		mcpTokens, err := buildMCPTokens(hCfg.McpArgs, toolDeps.BaseURL+"/mcp/worker", toolDeps.Config.Server.McpSecret)
		if err != nil {
			return nil, fmt.Errorf("invalid mcp_args shell syntax: %w", err)
		}

		if err := ops.ensureBranchCreationSafe(spawnCtx, toolDeps.Config.Project.RepoPath, branch); err != nil {
			if errors.Is(err, worktree.ErrUnsafeBranchCreate) {
				return nil, fmt.Errorf("branch %q is unsafe to create: %w", branch, err)
			}
			return nil, err
		}

		// Build paths.
		repoPath := toolDeps.Config.Project.RepoPath
		worktreePath, err := config.ProjectWorktreePath(toolDeps.Config.Project, taskID)
		if err != nil {
			return nil, fmt.Errorf("resolve worktree path: %w", err)
		}

		// Get current HEAD as base ref.
		baseRef, err := ops.headRef(spawnCtx, repoPath)
		if err != nil {
			return nil, err
		}

		// Step 1: Create worktree.
		if err := ops.createWorktree(spawnCtx, repoPath, branch, worktreePath, baseRef); err != nil {
			return nil, fmt.Errorf("create worktree: %w", err)
		}

		workerID := workerIDFor(toolDeps, taskID)

		// Step 2: Create tmux window.
		if err := ops.createWindow(spawnCtx, toolDeps.Session, workerID, worktreePath); err != nil {
			cleanupSpawnResources(spawnCtx, ops, toolDeps, workerID, branch, taskID, repoPath, worktreePath, true)
			return nil, fmt.Errorf("create tmux window: %w", err)
		}

		// Step 3: Update ledger — use UpdateIfStatus to reject concurrent
		// modifications (e.g., a split_task that changed the status to "split"
		// between our preflight check and this write).
		updateErr := toolDeps.Ledger.UpdateIfStatus(taskID, "unstarted", map[string]any{
			"status":          "in_progress",
			"branch":          branch,
			"worker_id":       workerID,
			"harness":         resolvedHarness,
			"allowed_files":   allowedFiles,
			"forbidden_files": forbiddenFiles,
		})
		if updateErr != nil {
			cleanupSpawnResources(spawnCtx, ops, toolDeps, workerID, branch, taskID, repoPath, worktreePath, true)
			return nil, fmt.Errorf("update ledger: %w", updateErr)
		}
		promptTask, err := loadTaskByID(toolDeps.Ledger, taskID)
		if err != nil {
			rollbackSpawnAfterLedgerUpdate(spawnCtx, ops, toolDeps, workerID, branch, taskID, repoPath, worktreePath)
			return nil, fmt.Errorf("reload task after update: %w", err)
		}

		// Register worker.
		toolDeps.Registry.Register(worker.Info{
			TaskID:    taskID,
			WorkerID:  workerID,
			Harness:   resolvedHarness,
			StartedAt: time.Now(),
		})

		// Step 4: Launch harness with MCP args.
		// Rebuild the command by single-quoting each split token so that expanded
		// URL/secret values with shell metacharacters are not interpreted by the shell.
		harnessCmd := buildHarnessCommand(hCfg.Command, mcpTokens)
		if err := ops.sendKeys(spawnCtx, toolDeps.Session, workerID, harnessCmd); err != nil {
			rollbackSpawnAfterLedgerUpdate(spawnCtx, ops, toolDeps, workerID, branch, taskID, repoPath, worktreePath)
			return nil, fmt.Errorf("send harness command: %w", err)
		}
		if err := ops.waitForHarnessProcess(spawnCtx, toolDeps.Session, workerID, 2*time.Second); err != nil {
			if isContextDoneError(spawnCtx, err) {
				rollbackSpawnAfterLedgerUpdate(spawnCtx, ops, toolDeps, workerID, branch, taskID, repoPath, worktreePath)
				return nil, fmt.Errorf("wait for harness process: %w", err)
			}
			log.Printf("warn: wait for harness process: %v", err)
		}

		// Step 5: Send task prompt (best-effort — worker is already running).
		// If this fails, include prompt_sent:false in the response so the orchestrator
		// knows to call followup_worker to deliver the task description.
		prompt := buildWorkerPromptFromTaskWithDeps(toolDeps, promptTask, taskID, workerID, branch,
			worktreePath, toolDeps.Config.Project.ValidationCommand)
		promptSent := true
		if err := ops.sendKeys(spawnCtx, toolDeps.Session, workerID, prompt); err != nil {
			if isContextDoneError(spawnCtx, err) {
				rollbackSpawnAfterLedgerUpdate(spawnCtx, ops, toolDeps, workerID, branch, taskID, repoPath, worktreePath)
				return nil, fmt.Errorf("send task prompt: %w", err)
			}
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
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
		workerID, err := stringArg(args, "worker_id")
		if err != nil {
			return nil, err
		}

		// Find task in registry or ledger.
		// Always load branch from the ledger: the registry does not store it.
		var taskID string
		if info, ok := toolDeps.Registry.Get(workerID); ok {
			taskID = info.TaskID
		}
		// Ledger search fills taskID as fallback when not in registry.
		tasks, err := toolDeps.Ledger.Load()
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			if t.WorkerID == workerID {
				if taskID == "" {
					taskID = t.ID
				}
				break
			}
		}

		// If neither the registry nor the ledger knows this worker, return an
		// error so the caller knows the ID was wrong. Best-effort kill the tmux
		// window in case it exists as an orphan.
		if taskID == "" {
			_ = tmux.KillWindow(toolDeps.Session, workerID)
			return nil, fmt.Errorf("worker %q not found in registry or ledger", workerID)
		}

		// Record cleanup as pending before touching external resources. If this
		// process exits between killing tmux and removing the worktree, list_tasks
		// still shows a retryable blocked task instead of an invisible orphan.
		intent := cleanupIntentStopWorker
		originalReason := ""
		prevTask, err := toolDeps.Ledger.UpdateIfStatusesReturnPrevWith(taskID,
			[]string{"in_progress", "blocked", "unstarted"},
			func(current ledger.Task) (map[string]any, error) {
				if current.WorkerID != "" && current.WorkerID != workerID {
					return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
						workerID, taskID, current.WorkerID)
				}
				if pendingIntent, pendingOriginalReason, ok := cleanupIntentFromTask(current); ok {
					intent = pendingIntent
					originalReason = pendingOriginalReason
				}
				fields := map[string]any{
					"status": "blocked",
					"reason": cleanupPendingReason(intent, nil, originalReason),
				}
				if current.WorkerID == "" {
					fields["worker_id"] = workerID
				}
				return fields, nil
			})
		if err != nil {
			return nil, err
		}
		cleanupWorkerID := prevTask.WorkerID
		if cleanupWorkerID == "" {
			cleanupWorkerID = workerID
		}
		result := cleanupWorkerResources(toolDeps, cleanupWorkerID, prevTask.Branch, taskID, intentDeletesBranch(intent))
		if cleanupErr := result.Err(); cleanupErr != nil {
			if err := markCleanupPending(toolDeps, taskID, cleanupWorkerID, prevTask, intent, originalReason, cleanupErr); err != nil {
				return nil, fmt.Errorf("%w; additionally failed to record cleanup state: %v", cleanupErr, err)
			}
			return nil, cleanupErr
		}
		if err := finishPendingCleanup(toolDeps, taskID, cleanupWorkerID, intent, originalReason); err != nil {
			return nil, err
		}
		if notifyType, ok := notifyTypeForCleanupIntent(intent); ok {
			if err := triggerAfterNotify(ctx, toolDeps, notifyType, taskID); err != nil {
				log.Printf("warn: stop_worker finalized %s cleanup for task %s but failed to trigger orchestrator: %v",
					notifyType, taskID, err)
			}
		}

		return map[string]any{"stopped": workerID}, nil
	}
}

func handleFollowupWorker(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
		workerID, err := stringArg(args, "worker_id")
		if err != nil {
			return nil, err
		}
		message, err := stringArg(args, "message")
		if err != nil {
			return nil, err
		}

		if err := ensureWorkerTaskActive(toolDeps.Ledger, workerID); err != nil {
			return nil, err
		}

		// Verify window exists.
		windowName := workerID
		alive, err := tmux.IsWindowAlive(toolDeps.Session, windowName)
		if err != nil {
			return nil, fmt.Errorf("check window: %w", err)
		}
		if !alive {
			return nil, fmt.Errorf("tmux window %q does not exist", windowName)
		}
		if err := ensureWorkerTaskActive(toolDeps.Ledger, workerID); err != nil {
			return nil, err
		}

		if err := tmux.SendKeys(toolDeps.Session, windowName, message); err != nil {
			return nil, fmt.Errorf("send keys: %w", err)
		}
		return map[string]any{"sent": true}, nil
	}
}

func handleGetPRStatus(deps *Deps) ToolHandler {
	return func(ctx context.Context, args map[string]any) (any, error) {
		toolDeps, err := depsForArgs(deps, args)
		if err != nil {
			return nil, err
		}
		prNumber, err := intArg(args, "pr_number")
		if err != nil {
			return nil, err
		}

		if prNumber <= 0 {
			return nil, fmt.Errorf("pr_number must be a positive integer")
		}
		if toolDeps.GitHub == nil {
			return nil, fmt.Errorf("github.token, github.owner, and github.repo must be configured")
		}
		return toolDeps.GitHub.GetPRStatus(ctx, prNumber)
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
		toolDeps, err := depsForPayload(deps, payload)
		if err != nil {
			return nil, err
		}

		taskID, err := stringArg(payload, "task_id")
		if err != nil {
			return nil, err
		}

		// All workers share the same bearer token, so require the caller's
		// worker_id and verify it again inside each ledger update lock.
		callerWorkerID, err := stringArg(payload, "worker_id")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(callerWorkerID) == "" {
			return nil, fmt.Errorf("worker_id is required")
		}

		tasks, err := toolDeps.Ledger.Load()
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
		if task.WorkerID != callerWorkerID {
			return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
				callerWorkerID, taskID, task.WorkerID)
		}
		if toolDeps.notifyAfterOwnershipPreflight != nil {
			toolDeps.notifyAfterOwnershipPreflight()
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
		if pendingIntent, _, ok := cleanupIntentFromTask(*task); ok {
			if expectedIntent, supported := cleanupIntentForNotifyType(notifyType); !supported || expectedIntent != pendingIntent {
				return nil, fmt.Errorf("task %s has %s cleanup pending; retry that cleanup before notify(%s)",
					taskID, pendingIntent, notifyType)
			}
		}

		switch notifyType {
		case "completed":
			rawPRURL := optionalStringArg(payload, "pr_url")
			if strings.ContainsAny(rawPRURL, "\r\n") {
				return nil, fmt.Errorf("pr_url must be a single line")
			}
			prURL := strings.TrimSpace(rawPRURL)
			if prURL == "" {
				return nil, fmt.Errorf("pr_url is required for completed notifications")
			}
			rawMergeCommit := optionalStringArg(payload, "merge_commit")
			if strings.ContainsAny(rawMergeCommit, "\r\n") {
				return nil, fmt.Errorf("merge_commit must be a single-line value")
			}
			mergeCommit := strings.TrimSpace(rawMergeCommit)
			if mergeCommit == "" {
				return nil, fmt.Errorf("merge_commit is required for completed notifications")
			}
			if !mergeCommitRE.MatchString(mergeCommit) {
				return nil, fmt.Errorf("merge_commit must be a valid commit hash")
			}

			prevTask, err := toolDeps.Ledger.UpdateIfStatusesReturnPrevWith(taskID, []string{"in_progress", "blocked"},
				func(current ledger.Task) (map[string]any, error) {
					if current.WorkerID != callerWorkerID {
						return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
							callerWorkerID, taskID, current.WorkerID)
					}
					if pendingIntent, _, ok := cleanupIntentFromTask(current); ok && pendingIntent != cleanupIntentCompleted {
						return nil, fmt.Errorf("task %s has %s cleanup pending; retry that cleanup before notify(completed)",
							taskID, pendingIntent)
					}
					fields := map[string]any{
						"status": "blocked",
						"pr_url": prURL,
						"reason": cleanupPendingReason(cleanupIntentCompleted, nil, ""),
					}
					// Append a fresh merge_commit comment so archive_task reads the
					// verified completion value, not a stale marker from the task body.
					comment := "<!-- merge_commit: " + mergeCommit + " -->"
					body := ledger.RemoveMergeCommitComment(current.Body)
					var nextBody string
					if strings.TrimSpace(body) == "" {
						nextBody = comment
					} else {
						nextBody = body + "\n" + comment
					}
					fields["body"] = bodyWithCleanupPending(nextBody, cleanupIntentCompleted, "")
					return fields, nil
				},
			)
			if err != nil {
				return nil, err
			}

			// Cleanup worker resources. The git branch is intentionally NOT deleted
			// here; it is preserved so archive_task can record it in the archive
			// front matter and then delete it. If archive_task is never called the
			// branch will be orphaned, but that matches the spec's deferred-deletion
			// design for completed tasks. If cleanup fails, the task remains blocked
			// with worker_id/branch/harness intact so notify(completed) or
			// stop_worker can retry without hiding the orphaned resources.
			wid := prevTask.WorkerID
			if wid == "" {
				wid = workerIDFor(toolDeps, taskID) // fallback
			}
			result := cleanupWorkerResources(toolDeps, wid, prevTask.Branch, taskID, false)
			if cleanupErr := result.Err(); cleanupErr != nil {
				if err := markCleanupPending(toolDeps, taskID, wid, prevTask, cleanupIntentCompleted, "", cleanupErr); err != nil {
					return nil, fmt.Errorf("%w; additionally failed to record cleanup state: %v", cleanupErr, err)
				}
				return nil, cleanupErr
			}
			if err := finishPendingCleanup(toolDeps, taskID, wid, cleanupIntentCompleted, ""); err != nil {
				return nil, err
			}

		case "blocked":
			reason, _ := payload["reason"].(string)
			if _, err := toolDeps.Ledger.UpdateIfStatusesReturnPrevWith(taskID, []string{"in_progress", "blocked"},
				func(current ledger.Task) (map[string]any, error) {
					if current.WorkerID != callerWorkerID {
						return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
							callerWorkerID, taskID, current.WorkerID)
					}
					if pendingIntent, _, ok := cleanupIntentFromTask(current); ok {
						return nil, fmt.Errorf("task %s has %s cleanup pending; retry that cleanup before notify(blocked)",
							taskID, pendingIntent)
					}
					fields := map[string]any{"status": "blocked"}
					if reason != "" {
						fields["reason"] = reason
					}
					return fields, nil
				},
			); err != nil {
				return nil, err
			}

		case "split_request":
			reason, _ := payload["reason"].(string)
			if pendingIntent, originalReason, ok := cleanupIntentFromTask(*task); ok && pendingIntent == cleanupIntentSplitRequest {
				pendingTask, err := toolDeps.Ledger.UpdateIfStatusesReturnPrevWith(taskID, []string{"blocked"},
					func(current ledger.Task) (map[string]any, error) {
						if current.WorkerID != callerWorkerID {
							return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
								callerWorkerID, taskID, current.WorkerID)
						}
						currentIntent, currentOriginalReason, ok := cleanupIntentFromTask(current)
						if !ok {
							return nil, fmt.Errorf("task %s is missing cleanup marker", taskID)
						}
						if currentIntent != cleanupIntentSplitRequest {
							return nil, fmt.Errorf("task %s has %s cleanup pending, not %s",
								taskID, currentIntent, cleanupIntentSplitRequest)
						}
						originalReason = currentOriginalReason
						return map[string]any{
							"reason": cleanupPendingReason(cleanupIntentSplitRequest, nil, originalReason),
						}, nil
					})
				if err != nil {
					return nil, err
				}
				wid := pendingTask.WorkerID
				if wid == "" {
					wid = workerIDFor(toolDeps, taskID)
				}
				result := cleanupWorkerResources(toolDeps, wid, pendingTask.Branch, taskID, true)
				if cleanupErr := result.Err(); cleanupErr != nil {
					if err := markCleanupPending(toolDeps, taskID, wid, pendingTask, cleanupIntentSplitRequest, originalReason, cleanupErr); err != nil {
						return nil, fmt.Errorf("%w; additionally failed to record cleanup state: %v", cleanupErr, err)
					}
					return nil, cleanupErr
				}
				if err := finishPendingCleanup(toolDeps, taskID, wid, cleanupIntentSplitRequest, originalReason); err != nil {
					return nil, err
				}
				break
			}
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
				childAllowed, err := fileListArg(sliceMap, "allowed_files")
				if err != nil {
					return nil, fmt.Errorf("proposed_slices[%d] allowed_files: %w", i, err)
				}
				childForbidden, err := fileListArg(sliceMap, "forbidden_files")
				if err != nil {
					return nil, fmt.Errorf("proposed_slices[%d] forbidden_files: %w", i, err)
				}
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
			srIDs, err := toolDeps.Ledger.GenerateIDs(len(pendingSRs))
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

			// Verify ownership under the parent update lock before writing child
			// tasks; a stale worker must not be able to mutate the ledger even
			// temporarily by adding children.
			prevTask, updateErr := toolDeps.Ledger.UpdateIfStatusesReturnPrevWith(taskID, []string{"in_progress", "blocked"},
				func(current ledger.Task) (map[string]any, error) {
					if current.WorkerID != callerWorkerID {
						return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
							callerWorkerID, taskID, current.WorkerID)
					}
					if pendingIntent, _, ok := cleanupIntentFromTask(current); ok {
						return nil, fmt.Errorf("task %s has %s cleanup pending; retry that cleanup before notify(split_request)",
							taskID, pendingIntent)
					}
					return map[string]any{
						"status": "blocked",
						"reason": cleanupPendingReason(cleanupIntentSplitRequest, nil, reason),
						"body":   bodyWithCleanupPending(current.Body, cleanupIntentSplitRequest, reason),
					}, nil
				},
			)
			if updateErr != nil {
				return nil, updateErr
			}

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
			if err := toolDeps.Ledger.AddAll(childTasks); err != nil {
				if _, rollbackErr := toolDeps.Ledger.UpdateIfStatusesReturnPrev(taskID, []string{"blocked"}, map[string]any{
					"status":    prevTask.Status,
					"worker_id": prevTask.WorkerID,
					"branch":    prevTask.Branch,
					"harness":   prevTask.Harness,
					"reason":    prevTask.Reason,
					"body":      prevTask.Body,
				}); rollbackErr != nil {
					log.Printf("warn: split_request rollback failed for task %s after child creation error: %v", taskID, rollbackErr)
				}
				return nil, err
			}

			// Cleanup resources using prevTask snapshot (not stale task load) so that
			// a concurrent stop_worker + spawn_worker that updated worker_id/branch
			// between the initial load and UpdateReturnPrev is handled correctly.
			// Parent state stays blocked until cleanup succeeds. The already-created
			// child tasks make retry idempotent via stop_worker or notify(split_request)
			// without allocating duplicate slices.
			wid := prevTask.WorkerID
			if wid == "" {
				wid = workerIDFor(toolDeps, taskID)
			}
			result := cleanupWorkerResources(toolDeps, wid, prevTask.Branch, taskID, true)
			if cleanupErr := result.Err(); cleanupErr != nil {
				if err := markCleanupPending(toolDeps, taskID, wid, prevTask, cleanupIntentSplitRequest, reason, cleanupErr); err != nil {
					return nil, fmt.Errorf("%w; additionally failed to record cleanup state: %v", cleanupErr, err)
				}
				return nil, cleanupErr
			}
			if err := finishPendingCleanup(toolDeps, taskID, wid, cleanupIntentSplitRequest, reason); err != nil {
				return nil, err
			}

		}

		if err := triggerAfterNotify(ctx, toolDeps, notifyType, taskID); err != nil {
			log.Printf("warn: notify %s for task %s updated ledger but failed to trigger orchestrator: %v",
				notifyType, taskID, err)
		}
		return map[string]any{"ok": true}, nil
	}
}

// ---- helpers ----

func projectProps(props map[string]any) map[string]any {
	if props == nil {
		props = make(map[string]any)
	}
	props["project_slug"] = prop("string", "Project slug")
	return props
}

func depsForArgs(deps *Deps, args map[string]any) (*Deps, error) {
	if deps.Manager == nil {
		return deps, nil
	}
	slug, err := stringArg(args, "project_slug")
	if err != nil {
		return nil, err
	}
	return depsForProject(deps, slug)
}

func depsForPayload(deps *Deps, payload map[string]any) (*Deps, error) {
	if deps.Manager == nil {
		return deps, nil
	}
	slug, err := stringArg(payload, "project_slug")
	if err != nil {
		return nil, err
	}
	return depsForProject(deps, slug)
}

func depsForProject(deps *Deps, slug string) (*Deps, error) {
	project, err := deps.Manager.Project(slug)
	if err != nil {
		return nil, err
	}
	var notifyTrigger notifyTriggerer
	if project.NotifyTrigger != nil {
		notifyTrigger = project.NotifyTrigger
	} else if project.Orchestrator != nil {
		notifyTrigger = project.Orchestrator
	}
	return &Deps{
		Ledger:        project.Ledger,
		Registry:      project.Registry,
		Config:        project.Config,
		GitHub:        project.GitHub,
		Session:       project.Session,
		BaseURL:       project.BaseURL,
		Manager:       deps.Manager,
		ProjectSlug:   project.Slug,
		NotifyTrigger: notifyTrigger,
		cleanup:       deps.cleanup,
		spawn:         deps.spawn,
	}, nil
}

func triggerAfterNotify(ctx context.Context, deps *Deps, notifyType, taskID string) error {
	if deps.NotifyTrigger == nil {
		return nil
	}
	return deps.NotifyTrigger.Trigger(context.WithoutCancel(ctx), notifyTriggerReason(notifyType, taskID))
}

func notifyTriggerReason(notifyType, taskID string) string {
	switch notifyType {
	case "completed":
		return "worker completed: " + taskID
	case "blocked":
		return "worker blocked: " + taskID
	case "split_request":
		return "worker split_request: " + taskID
	default:
		return "worker notify: " + taskID
	}
}

func fileListArg(args map[string]any, key string) ([]string, error) {
	if v, ok := args[key]; ok && v == nil {
		return nil, fmt.Errorf("argument %q must be an array of strings", key)
	}
	return stringSliceArg(args, key)
}

func workerIDFor(deps *Deps, taskID string) string {
	if deps.ProjectSlug != "" {
		return deps.ProjectSlug + "-worker-" + taskID
	}
	return "worker-" + taskID
}

func cleanupTaskBranch(repoPath, branch, taskID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTaskBranchTimeout)
	defer cancel()
	return cleanupTaskBranchContext(cleanupCtx, repoPath, branch, taskID)
}

func cleanupTaskBranchContext(ctx context.Context, repoPath, branch, taskID string) error {
	absRepoPath, err := filepath.Abs(repoPath)
	if err != nil {
		log.Printf("warn: worker cleanup failed to resolve repo path %q for task %s: %v", repoPath, taskID, err)
		return err
	}
	if err := worktree.DeleteTaskBranchIfSafeContext(ctx, absRepoPath, branch, taskID); err != nil {
		if errors.Is(err, worktree.ErrUnsafeBranchDelete) {
			log.Printf("warn: worker cleanup skipped unsafe branch delete for task %s: %v", taskID, err)
			return nil
		}
		if errors.Is(err, worktree.ErrOriginUnavailable) {
			log.Printf("warn: worker cleanup skipped branch delete for task %s (origin unavailable): %v", taskID, err)
			return nil
		}
		log.Printf("warn: worker cleanup failed to delete branch %q for task %s: %v", branch, taskID, err)
		return err
	}
	return nil
}

func cleanupSpawnResources(ctx context.Context, ops spawnWorkerOps, deps *Deps, workerID, branch, taskID, repoPath, worktreePath string, killWindow bool) {
	cleanupCtx, cancel := spawnRollbackContext(ctx)
	defer cancel()
	if killWindow && workerID != "" {
		_ = ops.killWindow(cleanupCtx, deps.Session, workerID)
	}
	if worktreePath != "" {
		_ = ops.removeWorktree(cleanupCtx, repoPath, worktreePath)
	}
	if branch != "" {
		_ = ops.deleteTaskBranch(cleanupCtx, repoPath, branch, taskID)
	}
}

func spawnRollbackContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), spawnWorkerRollbackTimeout)
}

func cleanupWorkerResources(deps *Deps, workerID, branch, taskID string, deleteBranch bool) workerCleanupResult {
	ops := deps.cleanup.withDefaults()
	result := workerCleanupResult{
		WorkerID: workerID,
		Branch:   branch,
	}
	if workerID != "" {
		alive, err := ops.isWindowAlive(deps.Session, workerID)
		if err != nil {
			result.TmuxErr = fmt.Errorf("check tmux window %q: %w", workerID, err)
		} else if alive {
			if err := ops.killWindow(deps.Session, workerID); err != nil {
				result.TmuxErr = fmt.Errorf("kill tmux window %q: %w", workerID, err)
			}
		}
	}
	if taskID != "" {
		wPath, err := config.ProjectWorktreePath(deps.Config.Project, taskID)
		if err != nil {
			// No safe path exists to report or remove when resolution fails.
			// Keep WorktreePath empty and surface the failure via WorktreeErr.
			result.WorktreeErr = fmt.Errorf("resolve worktree path: %w", err)
		} else if err := ops.removeWorktree(deps.Config.Project.RepoPath, wPath); err != nil {
			result.WorktreePath = wPath
			if !worktreeRemoveAlreadyDone(err) {
				result.WorktreeErr = fmt.Errorf("remove worktree %q: %w", wPath, err)
			}
		} else {
			result.WorktreePath = wPath
		}
	}
	if deleteBranch && branch != "" {
		if err := ops.deleteTaskBranch(deps.Config.Project.RepoPath, branch, taskID); err != nil {
			result.BranchErr = fmt.Errorf("delete branch %q: %w", branch, err)
		}
	}
	if !result.Failed() && workerID != "" {
		deps.Registry.Remove(workerID)
	}
	return result
}

func worktreeRemoveAlreadyDone(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is not a working tree")
}

func bodyWithCleanupPending(body string, intent cleanupIntent, originalReason string) string {
	body = removeCleanupPendingComment(body)
	marker, err := json.Marshal(cleanupPendingMarker{
		Intent:         string(intent),
		OriginalReason: originalReason,
	})
	if err != nil {
		return body
	}
	comment := cleanupPendingCommentStart + base64.StdEncoding.EncodeToString(marker) + cleanupPendingCommentEnd
	if strings.TrimSpace(body) == "" {
		return comment
	}
	return strings.TrimRight(body, "\n") + "\n" + comment
}

func removeCleanupPendingComment(body string) string {
	return cleanupPendingCommentRE.ReplaceAllString(body, "")
}

func cleanupIntentFromTask(task ledger.Task) (cleanupIntent, string, bool) {
	match := cleanupPendingCommentRE.FindStringSubmatch(task.Body)
	if len(match) != 2 {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(match[1])
	if err != nil {
		return "", "", false
	}
	var marker cleanupPendingMarker
	if err := json.Unmarshal(decoded, &marker); err != nil {
		return "", "", false
	}
	intent := cleanupIntent(marker.Intent)
	switch intent {
	case cleanupIntentStopWorker, cleanupIntentCompleted, cleanupIntentSplitRequest:
		return intent, marker.OriginalReason, true
	default:
		return "", "", false
	}
}

func cleanupPendingReason(intent cleanupIntent, cleanupErr error, originalReason string) string {
	detail := "cleanup in progress"
	if cleanupErr != nil {
		detail = cleanupErr.Error()
	}
	reason := fmt.Sprintf("cleanup pending after %s: %s", intent, detail)
	if intent == cleanupIntentSplitRequest {
		reason += cleanupOriginalSplitReasonPrefix + strconv.Quote(originalReason)
	}
	return reason
}

func cleanupIntentForNotifyType(notifyType string) (cleanupIntent, bool) {
	switch notifyType {
	case "completed":
		return cleanupIntentCompleted, true
	case "split_request":
		return cleanupIntentSplitRequest, true
	default:
		return "", false
	}
}

func notifyTypeForCleanupIntent(intent cleanupIntent) (string, bool) {
	switch intent {
	case cleanupIntentCompleted:
		return "completed", true
	case cleanupIntentSplitRequest:
		return "split_request", true
	default:
		return "", false
	}
}

func intentDeletesBranch(intent cleanupIntent) bool {
	return intent != cleanupIntentCompleted
}

func markCleanupPending(deps *Deps, taskID, workerID string, prevTask ledger.Task, intent cleanupIntent, originalReason string, cleanupErr error) error {
	_, err := deps.Ledger.UpdateIfStatusesReturnPrevWith(taskID, []string{"blocked"}, func(current ledger.Task) (map[string]any, error) {
		if current.WorkerID != "" && workerID != "" && current.WorkerID != workerID {
			return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
				workerID, taskID, current.WorkerID)
		}
		return map[string]any{
			"status":    "blocked",
			"worker_id": workerID,
			"branch":    prevTask.Branch,
			"harness":   prevTask.Harness,
			"reason":    cleanupPendingReason(intent, cleanupErr, originalReason),
		}, nil
	})
	return err
}

func finishPendingCleanup(deps *Deps, taskID, workerID string, intent cleanupIntent, originalReason string) error {
	_, err := deps.Ledger.UpdateIfStatusesReturnPrevWith(taskID, []string{"blocked"}, func(current ledger.Task) (map[string]any, error) {
		if current.WorkerID != "" && workerID != "" && current.WorkerID != workerID {
			return nil, fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
				workerID, taskID, current.WorkerID)
		}
		if pendingIntent, pendingOriginalReason, ok := cleanupIntentFromTask(current); ok {
			if pendingIntent != intent {
				return nil, fmt.Errorf("task %s has %s cleanup pending, not %s", taskID, pendingIntent, intent)
			}
			if intent == cleanupIntentSplitRequest && pendingOriginalReason != originalReason {
				return nil, fmt.Errorf("task %s split cleanup reason changed during retry", taskID)
			}
		} else if intent != cleanupIntentStopWorker {
			return nil, fmt.Errorf("task %s is missing %s cleanup marker", taskID, intent)
		}
		switch intent {
		case cleanupIntentCompleted:
			return map[string]any{
				"status":    "completed",
				"worker_id": "",
				"harness":   "",
				"reason":    "",
				"body":      removeCleanupPendingComment(current.Body),
			}, nil
		case cleanupIntentSplitRequest:
			return map[string]any{
				"status":    "split",
				"worker_id": "",
				"branch":    "",
				"harness":   "",
				"reason":    originalReason,
				"body":      removeCleanupPendingComment(current.Body),
			}, nil
		default:
			return map[string]any{
				"status":    "unstarted",
				"worker_id": "",
				"branch":    "",
				"harness":   "",
				"reason":    "",
				"body":      removeCleanupPendingComment(current.Body),
			}, nil
		}
	})
	return err
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
	return validateGitBranchNameContext(context.Background(), branch)
}

func validateGitBranchNameContext(ctx context.Context, branch string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", branch).CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("invalid branch %q: %s", branch, msg)
	}
	return nil
}

func gitBranchExists(repoPath, branch string) (bool, error) {
	return gitBranchExistsContext(context.Background(), repoPath, branch)
}

func gitBranchExistsContext(ctx context.Context, repoPath, branch string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).CombinedOutput()
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("check branch existence: %w", ctxErr)
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

func rollbackSpawnAfterLedgerUpdate(ctx context.Context, ops spawnWorkerOps, deps *Deps, workerID, branch, taskID, repoPath, worktreePath string) {
	cleanupSpawnResources(ctx, ops, deps, workerID, branch, taskID, repoPath, worktreePath, true)
	deps.Registry.Remove(workerID)
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
		log.Printf("warn: spawn_worker ledger rollback skipped for task %s: %v (concurrent modification already resolved the state)", taskID, rollbackErr)
	}
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

func waitForHarnessProcess(session, window string, timeout time.Duration) error {
	return waitForHarnessProcessContext(context.Background(), session, window, timeout)
}

func waitForHarnessProcessContext(ctx context.Context, session, window string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("harness process did not start within %v", timeout)
		}
		idle, err := tmux.IsPaneIdleContext(ctx, session, window)
		if err != nil {
			return err
		}
		if !idle {
			return nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func isContextDoneError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Is(err, ctxErr)
	}
	return false
}

func buildWorkerPromptFromTask(task *ledger.Task, taskID, workerID, branch, worktreePath, validationCmd string) string {
	return buildWorkerPromptWithDeps(nil, task, taskID, workerID, branch, task.AllowedFiles, task.ForbiddenFiles,
		worktreePath, validationCmd)
}

func buildWorkerPromptFromTaskWithDeps(deps *Deps, task *ledger.Task, taskID, workerID, branch, worktreePath, validationCmd string) string {
	return buildWorkerPromptWithDeps(deps, task, taskID, workerID, branch, task.AllowedFiles, task.ForbiddenFiles,
		worktreePath, validationCmd)
}

func buildWorkerPromptWithDeps(deps *Deps, task *ledger.Task, taskID, workerID, branch string, allowedFiles, forbiddenFiles []string,
	worktreePath, validationCmd string) string {
	var sb strings.Builder
	sb.WriteString("You are a Worker agent. Complete the following task:\n\n")
	if deps != nil && deps.ProjectSlug != "" {
		sb.WriteString("Project slug: " + deps.ProjectSlug + "\n")
	}
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
- Work only inside the Worktree path above; do not directly edit the parent repository checkout.
- Stop and report a blocker if the current checkout is not the Worktree path or Branch above.
- Do not rewrite history on a default branch or any branch that has an open pull request.
- Never force push. If an open-PR branch is behind its base, merge the base branch normally.
- Implement the task, validate, self-review, create a PR, and merge it.
- Do not call notify(type="completed") until the PR is merged, gh-review-hook has exited 0, and the merge commit is verified.
- Always include your worker_id in notify payloads for ownership verification.
- Always include project_slug in notify payloads when it is present above.
- When complete: call notify(type="completed", payload={project_slug, task_id, worker_id, pr_url, merge_commit}).
- If blocked: call notify(type="blocked", payload={project_slug, task_id, worker_id, reason}).
- If you need to split: call notify(type="split_request", payload={project_slug, task_id, worker_id, reason, proposed_slices}).
`)
	return sb.String()
}

// shellQuoteArg wraps a single argument in POSIX single quotes so that shell
// metacharacters in expanded values (URL, secret) are not interpreted.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
