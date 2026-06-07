package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
	runtimepkg "github.com/xpadev/ccx-t2/internal/runtime"
	"github.com/xpadev/ccx-t2/internal/worker"
)

func TestGetTasksIncludesBody(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{
		ID:           "task-001",
		Title:        "Build API",
		Status:       "unstarted",
		AllowedFiles: []string{"server/internal/web/**"},
		Body:         "Implement read endpoints.",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodGet, "/api/tasks")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var tasks []taskResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].Body != "Implement read endpoints." {
		t.Fatalf("Body = %q, want ledger markdown body", tasks[0].Body)
	}
	if len(tasks[0].AllowedFiles) != 1 || tasks[0].AllowedFiles[0] != "server/internal/web/**" {
		t.Fatalf("AllowedFiles = %#v, want server/internal/web/**", tasks[0].AllowedFiles)
	}
}

func TestProjectScopedTasksUseSelectedProjectLedger(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Runtime = config.RuntimeConfig{TmuxSession: "ccx-test", WorktreeBase: filepath.Join(dir, "worktrees")}
	cfg.Projects = map[string]config.ProjectConfig{
		"alpha": {
			Slug:         "alpha",
			RepoPath:     filepath.Join(dir, "alpha"),
			LedgerPath:   filepath.Join(dir, "alpha", "tasks", "ledger.md"),
			WorktreeBase: cfg.Runtime.WorktreeBase,
			Orchestrator: cfg.Orchestrator,
			GitHub:       cfg.GitHub,
		},
		"beta": {
			Slug:         "beta",
			RepoPath:     filepath.Join(dir, "beta"),
			LedgerPath:   filepath.Join(dir, "beta", "tasks", "ledger.md"),
			WorktreeBase: cfg.Runtime.WorktreeBase,
			Orchestrator: cfg.Orchestrator,
			GitHub:       cfg.GitHub,
		},
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
	if err := alpha.Ledger.Add(ledger.Task{ID: "task-20260101-0001", Title: "Alpha", Status: "unstarted"}); err != nil {
		t.Fatalf("Add alpha: %v", err)
	}
	if err := beta.Ledger.Add(ledger.Task{ID: "task-20260101-0002", Title: "Beta", Status: "unstarted"}); err != nil {
		t.Fatalf("Add beta: %v", err)
	}

	handler := New(Deps{Config: cfg, Manager: manager, AuthDisabled: true})
	resp := performRequest(handler, http.MethodGet, "/api/projects/alpha/tasks")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	var tasks []taskResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "Alpha" {
		t.Fatalf("tasks = %#v, want only Alpha task", tasks)
	}

	missing := performRequest(handler, http.MethodGet, "/api/projects/missing/tasks")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want %d", missing.Code, http.StatusNotFound)
	}
}

func TestPostTasksCreatesTaskAndTriggersOrchestrator(t *testing.T) {
	l := newTestLedger(t)
	trigger := &fakeTrigger{}
	server := New(Deps{Ledger: l, Trigger: trigger, AuthDisabled: true})

	resp := performJSONRequest(server, http.MethodPost, "/api/tasks", `{
		"title": "New task",
		"body": "Build mutation endpoint.",
		"allowed_files": ["server/internal/web/**"]
	}`)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}

	var created taskCreateResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if created.Task.ID == "" {
		t.Fatal("created task ID is empty")
	}
	if got := resp.Header().Get("Idempotency-Key"); got != created.Task.ID {
		t.Fatalf("Idempotency-Key = %q, want created task ID %q", got, created.Task.ID)
	}
	if created.Task.Status != "unstarted" {
		t.Fatalf("status = %q, want unstarted", created.Task.Status)
	}
	if !created.OrchestratorTriggered {
		t.Fatal("OrchestratorTriggered = false, want true")
	}
	if len(trigger.reasons) != 1 || !strings.Contains(trigger.reasons[0], created.Task.ID) {
		t.Fatalf("trigger reasons = %#v, want created task ID", trigger.reasons)
	}
}

func TestPostTasksSupportsIdempotencyKey(t *testing.T) {
	l := newTestLedger(t)
	server := New(Deps{Ledger: l, AuthDisabled: true})
	body := `{"idempotency_key":"task-20260101-0001","title":"New task"}`

	first := performJSONRequest(server, http.MethodPost, "/api/tasks", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want %d; body=%s", first.Code, http.StatusCreated, first.Body.String())
	}
	second := performJSONRequest(server, http.MethodPost, "/api/tasks", body)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d; body=%s", second.Code, http.StatusOK, second.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].ID != "task-20260101-0001" {
		t.Fatalf("task ID = %q, want task-20260101-0001", tasks[0].ID)
	}
}

func TestPostTasksRejectsInvalidIdempotencyKey(t *testing.T) {
	resp := performJSONRequest(New(Deps{Ledger: newTestLedger(t), AuthDisabled: true}), http.MethodPost, "/api/tasks", `{
		"idempotency_key": "client-task-1",
		"title": "New task"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestPostTasksDefaultsEmptyStatus(t *testing.T) {
	resp := performJSONRequest(New(Deps{Ledger: newTestLedger(t), AuthDisabled: true}), http.MethodPost, "/api/tasks", `{
		"title": "New task",
		"status": ""
	}`)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}

	var created taskCreateResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Task.Status != "unstarted" {
		t.Fatalf("task status = %q, want unstarted", created.Task.Status)
	}
}

func TestPostTasksRejectsDeletingStatus(t *testing.T) {
	resp := performJSONRequest(New(Deps{Ledger: newTestLedger(t), AuthDisabled: true}), http.MethodPost, "/api/tasks", `{
		"title": "New task",
		"status": "deleting"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestPostTasksDuplicateIdempotencyKeyReturnsExistingTask(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-20260101-0001", Title: "Existing", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPost, "/api/tasks", `{
		"idempotency_key": "task-20260101-0001",
		"title": "Duplicate"
	}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var created taskCreateResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Task.Title != "Existing" {
		t.Fatalf("task title = %q, want Existing", created.Task.Title)
	}
}

func TestPostTasksRejectsArchivedIdempotencyKey(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-20260101-0001", Title: "Existing", Status: "completed"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := l.Archive("task-20260101-0001", "abc123"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPost, "/api/tasks", `{
		"idempotency_key": "task-20260101-0001",
		"title": "Duplicate"
	}`)
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusConflict, resp.Body.String())
	}
}

func TestPostTasksIdempotentRetryRetriesTrigger(t *testing.T) {
	l := newTestLedger(t)
	trigger := &fakeTrigger{err: errors.New("tmux unavailable")}
	server := New(Deps{Ledger: l, Trigger: trigger, AuthDisabled: true})
	body := `{"idempotency_key":"task-20260101-0001","title":"New task"}`

	first := performJSONRequest(server, http.MethodPost, "/api/tasks", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d; body=%s", first.Code, http.StatusAccepted, first.Body.String())
	}
	trigger.err = nil
	second := performJSONRequest(server, http.MethodPost, "/api/tasks", body)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d; body=%s", second.Code, http.StatusOK, second.Body.String())
	}
	if len(trigger.reasons) != 2 {
		t.Fatalf("trigger count = %d, want 2", len(trigger.reasons))
	}

	var retry taskCreateResponse
	if err := json.Unmarshal(second.Body.Bytes(), &retry); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if !retry.OrchestratorTriggered {
		t.Fatal("retry OrchestratorTriggered = false, want true")
	}
}

func TestPostTasksReturnsAcceptedWhenTriggerFails(t *testing.T) {
	l := newTestLedger(t)
	server := New(Deps{
		Ledger:       l,
		Trigger:      &fakeTrigger{err: errors.New("tmux unavailable")},
		AuthDisabled: true,
	})

	resp := performJSONRequest(server, http.MethodPost, "/api/tasks", `{"title":"New task"}`)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusAccepted, resp.Body.String())
	}

	var created taskCreateResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if created.Task.ID == "" {
		t.Fatal("created task ID is empty")
	}
	if created.TriggerError == "" {
		t.Fatal("TriggerError is empty, want trigger failure marker")
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
}

func TestPatchTaskUpdatesFields(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted", Body: "old body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{
		"title": "Updated",
		"status": "blocked",
		"reason": "Needs detail",
		"body": "new body"
	}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var updated taskResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated task: %v", err)
	}
	if updated.Title != "Updated" || updated.Status != "blocked" || updated.Reason != "Needs detail" || updated.Body != "new body" {
		t.Fatalf("updated task = %#v, want patched fields", updated)
	}
	if updated.UpdatedAt != "" {
		t.Fatalf("updated_at = %q, want omitted from PATCH response", updated.UpdatedAt)
	}
}

func TestPatchTaskNormalizesBodyNewlines(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{"body":"\nhello\n"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var updated taskResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated task: %v", err)
	}
	if updated.Body != "hello" {
		t.Fatalf("body = %q, want hello", updated.Body)
	}
}

func TestPatchRejectsEmptyTitleAndBody(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted", Body: "old body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{
		"title": "",
		"body": ""
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestPatchTaskNotFound(t *testing.T) {
	resp := performJSONRequest(New(Deps{Ledger: newTestLedger(t), AuthDisabled: true}), http.MethodPatch, "/api/tasks/missing", `{"title":"Updated"}`)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusNotFound)
	}
}

func TestPatchRejectsIdempotencyKey(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{
		"idempotency_key": "task-20260101-0001",
		"title": "Updated"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestPatchRejectsEmptyStatus(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{"status": ""}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestPatchRejectsDeletingStatus(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{"status": "deleting"}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Status != "unstarted" {
		t.Fatalf("tasks = %#v, want status unchanged", tasks)
	}
}

func TestDeleteTaskRemovesTask(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Delete me", Status: "unstarted", Body: "body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodDelete, "/api/tasks/task-001")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	var deleted taskDeleteResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if deleted.Deleted.ID != "task-001" || deleted.Deleted.Body != "body" {
		t.Fatalf("deleted = %#v, want task-001 with body", deleted.Deleted)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0", len(tasks))
	}
}

func TestDeleteTaskNotFound(t *testing.T) {
	resp := performRequest(New(Deps{Ledger: newTestLedger(t), AuthDisabled: true}), http.MethodDelete, "/api/tasks/missing")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusNotFound)
	}
}

func TestDeleteInProgressTaskCleansWorker(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{
		ID:       "task-001",
		Title:    "Delete worker",
		Status:   "in_progress",
		WorkerID: "worker-task-001",
		Branch:   "feature/task-001",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cleaner := &fakeCleaner{}

	resp := performRequest(New(Deps{Ledger: l, Cleaner: cleaner, AuthDisabled: true}), http.MethodDelete, "/api/tasks/task-001")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if len(cleaner.tasks) != 1 || cleaner.tasks[0].WorkerID != "worker-task-001" {
		t.Fatalf("cleaner tasks = %#v, want worker-task-001", cleaner.tasks)
	}
	var deleted taskDeleteResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if !deleted.WorkerCleaned {
		t.Fatal("WorkerCleaned = false, want true")
	}
}

func TestDeleteInProgressTaskReportsCleanupFailure(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Delete worker", Status: "in_progress", WorkerID: "worker-task-001"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performRequest(New(Deps{
		Ledger:       l,
		Cleaner:      &fakeCleaner{err: errors.New("tmux unavailable")},
		AuthDisabled: true,
	}), http.MethodDelete, "/api/tasks/task-001")
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusAccepted, resp.Body.String())
	}
	var deleted taskDeleteResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if deleted.CleanupError == "" {
		t.Fatal("CleanupError is empty, want cleanup failure marker")
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task-001" {
		t.Fatalf("tasks after cleanup failure = %#v, want task-001 retained", tasks)
	}
	if tasks[0].Status != "in_progress" || tasks[0].WorkerID != "worker-task-001" {
		t.Fatalf("restored task = %#v, want original worker metadata", tasks[0])
	}
}

func TestDeleteInProgressTaskReservesIDDuringCleanup(t *testing.T) {
	l := newTestLedger(t)
	id, err := l.AddNew(ledger.Task{Title: "Delete worker", Status: "in_progress", WorkerID: "worker-task"})
	if err != nil {
		t.Fatalf("AddNew: %v", err)
	}
	cleaner := &fakeCleaner{
		fn: func() error {
			newID, err := l.AddNew(ledger.Task{Title: "Concurrent create", Status: "unstarted"})
			if err != nil {
				return err
			}
			if newID == id {
				return errors.New("concurrent create reused deleting task ID")
			}
			return nil
		},
	}

	resp := performRequest(New(Deps{Ledger: l, Cleaner: cleaner, AuthDisabled: true}), http.MethodDelete, "/api/tasks/"+id)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "Concurrent create" {
		t.Fatalf("tasks after delete = %#v, want only concurrent create", tasks)
	}
}

func TestDeleteInProgressTaskRejectsConcurrentCleanup(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Delete worker", Status: "in_progress", WorkerID: "worker-task-001"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	var handler http.Handler
	cleaner := &fakeCleaner{}
	cleaner.fn = func() error {
		resp := performRequest(handler, http.MethodDelete, "/api/tasks/task-001")
		if resp.Code != http.StatusAccepted {
			return fmt.Errorf("concurrent status = %d, want %d; body=%s", resp.Code, http.StatusAccepted, resp.Body.String())
		}
		var deleted taskDeleteResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &deleted); err != nil {
			return fmt.Errorf("decode concurrent delete response: %w", err)
		}
		if !deleted.DeletePending {
			return errors.New("DeletePending = false, want true")
		}
		return nil
	}
	handler = New(Deps{Ledger: l, Cleaner: cleaner, AuthDisabled: true})

	resp := performRequest(handler, http.MethodDelete, "/api/tasks/task-001")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if len(cleaner.tasks) != 1 {
		t.Fatalf("cleanup calls = %d, want 1", len(cleaner.tasks))
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks after delete = %#v, want empty ledger", tasks)
	}
}

func TestPatchDeletingTaskRejectedDuringCleanup(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Delete worker", Status: "in_progress", WorkerID: "worker-task-001"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	var handler http.Handler
	cleaner := &fakeCleaner{}
	cleaner.fn = func() error {
		resp := performJSONRequest(handler, http.MethodPatch, "/api/tasks/task-001", `{"title":"mutated"}`)
		if resp.Code != http.StatusConflict {
			return fmt.Errorf("patch status = %d, want %d; body=%s", resp.Code, http.StatusConflict, resp.Body.String())
		}
		return nil
	}
	handler = New(Deps{Ledger: l, Cleaner: cleaner, AuthDisabled: true})

	resp := performRequest(handler, http.MethodDelete, "/api/tasks/task-001")
	if resp.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if len(cleaner.tasks) != 1 {
		t.Fatalf("cleanup calls = %d, want 1", len(cleaner.tasks))
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks after delete = %#v, want empty ledger", tasks)
	}
}

func TestDeletingMarkerWithNanoTimestampIsFresh(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 123456789, time.UTC)
	task := ledger.Task{
		ID:        "task-001",
		Status:    "deleting",
		UpdatedAt: now.Format(time.RFC3339Nano),
	}

	if taskNeedsDeleteCleanup(task, now.Add(time.Minute)) {
		t.Fatal("taskNeedsDeleteCleanup = true, want fresh deleting marker to be pending")
	}
}

func TestDeleteStaleDeletingTaskRetriesCleanup(t *testing.T) {
	l := newTestLedger(t)
	task := ledger.Task{
		ID:        "task-001",
		Title:     "Delete worker",
		Status:    "deleting",
		WorkerID:  "worker-task-001",
		UpdatedAt: time.Now().Add(-deleteCleanupLease - time.Minute).Format(time.RFC3339),
	}
	if err := l.RestoreTaskSnapshot(task); err != nil {
		t.Fatalf("RestoreTaskSnapshot: %v", err)
	}
	cleaner := &fakeCleaner{}

	resp := performRequest(New(Deps{Ledger: l, Cleaner: cleaner, AuthDisabled: true}), http.MethodDelete, "/api/tasks/task-001")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if len(cleaner.tasks) != 1 || cleaner.tasks[0].Status != "deleting" {
		t.Fatalf("cleaner tasks = %#v, want stale deleting task", cleaner.tasks)
	}
}

func TestDeleteExpiredMarkerTakeoverDoesNotRestoreStaleCleanupFailure(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Delete worker", Status: "in_progress", WorkerID: "worker-task-001"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	var handler http.Handler
	cleaner := &fakeCleaner{}
	cleaner.fn = func() error {
		if len(cleaner.tasks) == 1 {
			tasks, err := l.Load()
			if err != nil {
				return err
			}
			if len(tasks) != 1 || tasks[0].Status != "deleting" {
				return fmt.Errorf("tasks before takeover = %#v, want deleting marker", tasks)
			}
			staleMarker := tasks[0]
			staleMarker.UpdatedAt = time.Now().Add(-deleteCleanupLease - time.Minute).Format(time.RFC3339)
			if err := l.RestoreTaskSnapshot(staleMarker); err != nil {
				return err
			}
			resp := performRequest(handler, http.MethodDelete, "/api/tasks/task-001")
			if resp.Code != http.StatusOK {
				return fmt.Errorf("takeover status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
			}
			return errors.New("original cleanup failed after takeover")
		}
		return nil
	}
	handler = New(Deps{Ledger: l, Cleaner: cleaner, AuthDisabled: true})

	resp := performRequest(handler, http.MethodDelete, "/api/tasks/task-001")
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusAccepted, resp.Body.String())
	}
	var deleted taskDeleteResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if deleted.CleanupError == "" || !deleted.DeletePending {
		t.Fatalf("delete response = %#v, want cleanup_error and delete_pending", deleted)
	}
	if len(cleaner.tasks) != 2 {
		t.Fatalf("cleanup calls = %d, want 2", len(cleaner.tasks))
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks after takeover = %#v, want empty ledger", tasks)
	}
}

func TestDeleteBlockedTaskCleansWorker(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{
		ID:       "task-001",
		Title:    "Delete blocked worker",
		Status:   "blocked",
		WorkerID: "worker-task-001",
		Branch:   "feature/task-001",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cleaner := &fakeCleaner{}

	resp := performRequest(New(Deps{Ledger: l, Cleaner: cleaner, AuthDisabled: true}), http.MethodDelete, "/api/tasks/task-001")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if len(cleaner.tasks) != 1 || cleaner.tasks[0].Status != "blocked" {
		t.Fatalf("cleaner tasks = %#v, want blocked task", cleaner.tasks)
	}
}

func TestCleanupMissingResourceErrorsAreRetrySafe(t *testing.T) {
	for _, err := range []error{
		errors.New("tmux [kill-window -t session:worker-task-001]: exit status 1: can't find window: worker-task-001"),
		errors.New("git [-C /repo worktree remove --force /tmp/missing]: exit status 128: '/tmp/missing' is not a working tree"),
		errors.New("git [-C /repo branch -D feature/task-001]: exit status 1: error: branch 'feature/task-001' not found"),
		errors.New("git branch -D feature/task-001: exit status 1: error: branch 'feature/task-001' not found"),
	} {
		if !(isMissingTmuxWindowError(err) || isMissingWorktreeError(err) || isMissingBranchError(err)) {
			t.Fatalf("error %q was not classified as retry-safe missing resource", err)
		}
	}
	if isMissingWorktreeError(errors.New("stat /bad/repo: no such file or directory")) {
		t.Fatal("repo path error was classified as missing worktree")
	}
}

func TestPostTasksRejectsEmptyTask(t *testing.T) {
	resp := performJSONRequest(New(Deps{Ledger: newTestLedger(t), AuthDisabled: true}), http.MethodPost, "/api/tasks", `{}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestPostTasksRejectsOversizedJSON(t *testing.T) {
	body := `{"title":"` + strings.Repeat("x", 1<<20) + `"}`
	resp := performJSONRequest(New(Deps{Ledger: newTestLedger(t), AuthDisabled: true}), http.MethodPost, "/api/tasks", body)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestGetWorkersReturnsSortedSnapshot(t *testing.T) {
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-b", TaskID: "task-002", Harness: "codex", StartedAt: time.Unix(2, 0).UTC()})
	registry.Register(worker.Info{WorkerID: "worker-a", TaskID: "task-001", Harness: "codex", StartedAt: time.Unix(1, 0).UTC()})

	resp := performRequest(New(Deps{Registry: registry, AuthDisabled: true}), http.MethodGet, "/api/workers")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var workers []workerResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &workers); err != nil {
		t.Fatalf("decode workers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("len(workers) = %d, want 2", len(workers))
	}
	if got := []string{workers[0].WorkerID, workers[1].WorkerID}; got[0] != "worker-a" || got[1] != "worker-b" {
		t.Fatalf("worker order = %#v, want worker-a, worker-b", got)
	}
}

func TestGetHarnessesUsesConfiguredWorkerHarnesses(t *testing.T) {
	cfg := testConfig()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cfg.Harnesses["worker"] = config.HarnessConfig{
		Command: exe,
		McpArgs: "--mcp-url {url} --token nested-secret-value",
	}

	resp := performRequest(New(Deps{Config: cfg, AuthDisabled: true}), http.MethodGet, "/api/harnesses")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	for _, secret := range []string{"nested-secret-value", "mcp_args", "McpArgs", "token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("harness response leaked %q in %s", secret, body)
		}
	}

	var harnesses []struct {
		Name      string       `json:"name"`
		Available bool         `json:"available"`
		Usage     harnessUsage `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &harnesses); err != nil {
		t.Fatalf("decode harnesses: %v", err)
	}
	if len(harnesses) != 1 {
		t.Fatalf("len(harnesses) = %d, want 1", len(harnesses))
	}
	if harnesses[0].Name != "worker" || !harnesses[0].Available {
		t.Fatalf("harness = %#v, want available worker", harnesses[0])
	}
	if harnesses[0].Usage.Command != exe {
		t.Fatalf("usage command = %q, want %s", harnesses[0].Usage.Command, exe)
	}
}

func TestGetConfigRedactsSecrets(t *testing.T) {
	cfg := testConfig()
	cfg.Server.McpSecret = "mcp-secret-value"
	cfg.GitHub.Token = "github-token-value"
	cfg.Project.ValidationCommand = "GITHUB_TOKEN=nested-validation-secret go test ./..."
	cfg.Harnesses["worker"] = config.HarnessConfig{
		Command: "sh",
		McpArgs: "--mcp-url {url} --token nested-secret-value --secret {secret}",
	}

	resp := performRequest(New(Deps{Config: cfg, AuthDisabled: true}), http.MethodGet, "/api/config")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	for _, secret := range []string{"mcp-secret-value", "github-token-value", "nested-secret-value", "nested-validation-secret", "mcp_secret", "mcp_args", "McpArgs", "token", "validation_command"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response leaked %q in %s", secret, body)
		}
	}
	for _, want := range []string{"repo_path", "worktree_base", "heartbeat_interval"} {
		if !strings.Contains(body, want) {
			t.Fatalf("config response = %s, want snake_case key %q", body, want)
		}
	}

	var cfgResp configResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &cfgResp); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfgResp.Server.Port != 9090 {
		t.Fatalf("server.port = %d, want 9090", cfgResp.Server.Port)
	}
	if cfgResp.GitHub.Owner != "xpadev-net" || cfgResp.GitHub.Repo != "ccx-t2" {
		t.Fatalf("github = %#v, want owner/repo", cfgResp.GitHub)
	}
	if got := cfgResp.Harnesses["worker"].Command; got != "sh" {
		t.Fatalf("worker harness command = %q, want sh", got)
	}
}

func TestPatchConfigUpdatesAndPersistsEditableFields(t *testing.T) {
	t.Setenv("CCX_TEST_SECRET", "secret-value")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Server.McpSecret = "${CCX_TEST_SECRET}"
	cfg.Project.ValidationCommand = "GITHUB_TOKEN=${GITHUB_TOKEN} go test ./..."
	cfg.Harnesses["worker"] = config.HarnessConfig{
		Command: "sh",
		McpArgs: "--mcp-url {url} --token {secret}",
	}
	cfg.Harnesses["orchestrator"] = config.HarnessConfig{
		Command: "sh",
		McpArgs: "--mcp-url {url} --token {secret}",
	}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, AuthDisabled: true})

	resp := performJSONRequest(server, http.MethodPatch, "/api/config", `{
		"project": {"slug": "next", "repo_path": "/repo2"},
		"server": {"port": 9091},
		"orchestrator": {"harness": "orchestrator", "heartbeat_interval": "2m", "timeout": "45m"},
		"worker_harnesses": ["worker"],
		"harnesses": {"worker": {"command": "sh"}},
		"github": {"owner": "org", "repo": "repo"}
	}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	for _, secret := range []string{"CCX_TEST_SECRET", "GITHUB_TOKEN", "mcp_secret", "mcp_args", "validation_command"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response leaked %q in %s", secret, body)
		}
	}
	var patched configResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if patched.Project.Slug != "next" || patched.Project.RepoPath != "/repo2" || patched.Server.Port != 9091 {
		t.Fatalf("patched config = %#v, want updated project/server", patched)
	}
	if patched.Orchestrator.HeartbeatInterval != "2m0s" || patched.Orchestrator.Timeout != "45m0s" {
		t.Fatalf("patched orchestrator = %#v, want updated durations", patched.Orchestrator)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config: %v", err)
	}
	for _, want := range []string{"mcp_secret: ${CCX_TEST_SECRET}", "validation_command: GITHUB_TOKEN=${GITHUB_TOKEN} go test ./...", "mcp_args: --mcp-url {url} --token {secret}"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("saved config missing %q in:\n%s", want, raw)
		}
	}
}

func TestPatchConfigRejectsInvalidDuration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	resp := performJSONRequest(New(Deps{Config: cfg, ConfigPath: configPath, AuthDisabled: true}), http.MethodPatch, "/api/config", `{
		"orchestrator": {"heartbeat_interval": "soon"}
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func TestPatchConfigRequiresConfigPath(t *testing.T) {
	resp := performJSONRequest(New(Deps{Config: testConfig(), AuthDisabled: true}), http.MethodPatch, "/api/config", `{
		"server": {"port": 9091}
	}`)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusInternalServerError, resp.Body.String())
	}
}

func TestPatchConfigConcurrentRequestsPreserveUpdates(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, AuthDisabled: true})

	errs := make(chan error, 3)
	go func() {
		resp := performJSONRequest(server, http.MethodPatch, "/api/config", `{"project":{"slug":"concurrent"}}`)
		if resp.Code != http.StatusOK {
			errs <- fmt.Errorf("project patch status = %d body=%s", resp.Code, resp.Body.String())
			return
		}
		errs <- nil
	}()
	go func() {
		resp := performJSONRequest(server, http.MethodPatch, "/api/config", `{"github":{"owner":"org2"}}`)
		if resp.Code != http.StatusOK {
			errs <- fmt.Errorf("github patch status = %d body=%s", resp.Code, resp.Body.String())
			return
		}
		errs <- nil
	}()
	go func() {
		resp := performRequest(server, http.MethodGet, "/api/config")
		if resp.Code != http.StatusOK {
			errs <- fmt.Errorf("get config status = %d body=%s", resp.Code, resp.Body.String())
			return
		}
		errs <- nil
	}()
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load final config: %v", err)
	}
	if reloaded.Project.Slug != "concurrent" || reloaded.GitHub.Owner != "org2" {
		t.Fatalf("final config = %#v, want both concurrent updates preserved", reloaded)
	}
}

func TestPatchConfigDoesNotMutateCallerConfigPointer(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, AuthDisabled: true})

	resp := performJSONRequest(server, http.MethodPatch, "/api/config", `{"project":{"slug":"owned"}}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if loaded.Project.Slug != "ccx-t2" {
		t.Fatalf("caller config slug = %q, want original ccx-t2", loaded.Project.Slug)
	}
}

func TestWorkerLogWebSocketStreamsLines(t *testing.T) {
	lines := make(chan string, 2)
	lines <- "hello"
	lines <- "world"
	close(lines)
	cleanupCalled := make(chan struct{})
	server := httptest.NewServer(New(Deps{
		Config: testConfig(),
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			if session != "ccx-t2" || window != "worker-task-001" {
				t.Fatalf("pipe args = %q %q, want ccx-t2 worker-task-001", session, window)
			}
			return lines, func() { close(cleanupCalled) }, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/worker/worker-task-001"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	for _, want := range []string{"hello", "world"} {
		var msg wsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("ReadJSON: %v", err)
		}
		if msg.Type != "line" || msg.Data != want {
			t.Fatalf("message = %#v, want line %q", msg, want)
		}
	}
	var closed wsMessage
	if err := conn.ReadJSON(&closed); err != nil {
		t.Fatalf("ReadJSON closed: %v", err)
	}
	if closed.Type != "closed" {
		t.Fatalf("closed message = %#v, want closed", closed)
	}
	select {
	case <-cleanupCalled:
	case <-time.After(time.Second):
		t.Fatal("cleanup was not called")
	}
}

func TestWorkerLogWebSocketAcceptsTokenQueryAuth(t *testing.T) {
	lines := make(chan string)
	close(lines)
	server := httptest.NewServer(New(Deps{
		Config: testConfig(),
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			return lines, func() {}, nil
		},
		Secret: "web-secret",
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/worker/worker-task-001?token=web-secret"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	var msg wsMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if msg.Type != "closed" {
		t.Fatalf("message = %#v, want closed", msg)
	}
}

func TestWebSocketRejectsDisallowedOrigin(t *testing.T) {
	server := httptest.NewServer(New(Deps{
		Config:         testConfig(),
		AuthDisabled:   true,
		AllowedOrigins: []string{"http://allowed.example"},
	}))
	defer server.Close()
	header := http.Header{"Origin": []string{"http://evil.example"}}

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/ledger"), header)
	if err == nil {
		t.Fatal("Dial error = nil, want origin rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %#v, want 403", resp)
	}
}

func TestWorkerLogWebSocketRejectsSecondSubscriber(t *testing.T) {
	lines := make(chan string)
	server := httptest.NewServer(New(Deps{
		Config: testConfig(),
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			return lines, func() {}, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()
	first, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/worker/worker-task-001"), nil)
	if err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	defer first.Close()

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/worker/worker-task-001"), nil)
	if err == nil {
		t.Fatal("second Dial error = nil, want conflict")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("response = %#v, want 409", resp)
	}
	close(lines)
}

func TestLedgerWebSocketReceivesChangeNotification(t *testing.T) {
	l := newTestLedger(t)
	server := httptest.NewServer(New(Deps{Ledger: l, AuthDisabled: true}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/ledger"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	var ready wsMessage
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("ReadJSON ready: %v", err)
	}
	if ready.Type != "ready" {
		t.Fatalf("ready message = %#v, want ready", ready)
	}
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Changed", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	var changed wsMessage
	if err := conn.ReadJSON(&changed); err != nil {
		t.Fatalf("ReadJSON changed: %v", err)
	}
	if changed.Type != "ledger_changed" {
		t.Fatalf("changed message = %#v, want ledger_changed", changed)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	resp := performRequest(New(Deps{AuthDisabled: true}), http.MethodPut, "/api/tasks")
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusMethodNotAllowed)
	}
	if allow := resp.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("Allow = %q, want GET, POST", allow)
	}
}

func wsURL(baseURL, path string) string {
	return "ws" + strings.TrimPrefix(baseURL, "http") + path
}

func TestBearerAuth(t *testing.T) {
	l := newTestLedger(t)
	server := New(Deps{
		Ledger:         l,
		Secret:         "web-secret",
		AllowedOrigins: []string{"http://localhost:5173"},
	})

	resp := performRequestWithHeaders(server, http.MethodGet, "/api/tasks", map[string]string{
		"Origin": "http://localhost:5173",
	})
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("unauthorized Access-Control-Allow-Origin = %q, want http://localhost:5173", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Authorization", "Bearer web-secret")
	authorized := httptest.NewRecorder()
	server.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d; body=%s", authorized.Code, http.StatusOK, authorized.Body.String())
	}
	if got := authorized.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("authorized Access-Control-Allow-Origin = %q, want http://localhost:5173", got)
	}
}

func TestAuthRequiresSecretOrExplicitOptOut(t *testing.T) {
	l := newTestLedger(t)
	resp := performRequest(New(Deps{Ledger: l}), http.MethodGet, "/api/tasks")
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}
}

func TestCORSPreflight(t *testing.T) {
	server := New(Deps{Secret: "web-secret", AllowedOrigins: []string{"http://localhost:5173"}})

	resp := performRequestWithHeaders(server, http.MethodOptions, "/api/tasks", map[string]string{
		"Origin": "http://localhost:5173",
	})
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusNoContent)
	}
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want http://localhost:5173", got)
	}
	if got := resp.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}
	if got := resp.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, PATCH, DELETE, OPTIONS" {
		t.Fatalf("Access-Control-Allow-Methods = %q, want GET, POST, PATCH, DELETE, OPTIONS", got)
	}
	if got := resp.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Fatalf("Access-Control-Allow-Headers = %q, want Authorization", got)
	}
}

func newTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.md")
	if err := os.WriteFile(ledgerPath, nil, 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	return ledger.NewLedger(ledgerPath, filepath.Join(dir, "archive"))
}

func testConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{
			Slug:              "ccx-t2",
			RepoPath:          "/repo",
			WorktreeBase:      "/worktrees",
			ValidationCommand: "go test ./...",
		},
		Server: config.ServerConfig{
			Port: 9090,
		},
		Orchestrator: config.OrchestratorConfig{
			Harness:           "orchestrator",
			HeartbeatInterval: time.Minute,
			Timeout:           30 * time.Minute,
		},
		WorkerHarnesses: []string{"worker"},
		Harnesses: map[string]config.HarnessConfig{
			"worker": {
				Command: "sh",
				McpArgs: "--mcp-url {url}",
			},
			"orchestrator": {
				Command: "sh",
				McpArgs: "--mcp-url {url}",
			},
		},
		GitHub: config.GitHubConfig{
			Token: "github-token",
			Owner: "xpadev-net",
			Repo:  "ccx-t2",
		},
	}
}

func performRequest(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	return performRequestWithHeaders(handler, method, target, nil)
}

func performJSONRequest(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func performRequestWithHeaders(handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

type fakeTrigger struct {
	reasons []string
	err     error
}

func (f *fakeTrigger) Trigger(ctx context.Context, reason string) error {
	f.reasons = append(f.reasons, reason)
	return f.err
}

type fakeCleaner struct {
	tasks []ledger.Task
	err   error
	fn    func() error
}

func (f *fakeCleaner) CleanupWorker(ctx context.Context, task ledger.Task) error {
	f.tasks = append(f.tasks, task)
	if f.fn != nil {
		return f.fn()
	}
	return f.err
}
