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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	projectsResp := performRequest(handler, http.MethodGet, "/api/projects")
	if projectsResp.Code != http.StatusOK {
		t.Fatalf("projects status = %d, want %d; body=%s", projectsResp.Code, http.StatusOK, projectsResp.Body.String())
	}
	var projects []projectResponse
	if err := json.Unmarshal(projectsResp.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(projects) != 2 || projects[0].Session != "ccx-test" || projects[1].Session != "ccx-test" {
		t.Fatalf("projects = %#v, want project sessions", projects)
	}

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

func TestProjectPostTaskUsesSelectedProjectRuntimeAndNotifiesLedger(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Runtime = config.RuntimeConfig{TmuxSession: "ccx-test", WorktreeBase: filepath.Join(dir, "worktrees")}
	cfg.Orchestrator.Timeout = 0
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
	handler := New(Deps{Config: cfg, Manager: manager, AuthDisabled: true})
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/ledger"), nil)
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

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/tasks", `{
		"idempotency_key": "task-20260101-0001",
		"title": "Project task"
	}`)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusAccepted, resp.Body.String())
	}
	var created taskCreateResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if created.Task.ID != "task-20260101-0001" || created.TriggerError == "" {
		t.Fatalf("created = %#v, want project task with retryable trigger error", created)
	}
	alphaTasks, err := alpha.Ledger.Load()
	if err != nil {
		t.Fatalf("Load alpha: %v", err)
	}
	if len(alphaTasks) != 1 || alphaTasks[0].Title != "Project task" {
		t.Fatalf("alpha tasks = %#v, want created project task", alphaTasks)
	}
	betaTasks, err := beta.Ledger.Load()
	if err != nil {
		t.Fatalf("Load beta: %v", err)
	}
	if len(betaTasks) != 0 {
		t.Fatalf("beta tasks = %#v, want untouched beta ledger", betaTasks)
	}
	var changed wsMessage
	if err := conn.ReadJSON(&changed); err != nil {
		t.Fatalf("ReadJSON changed: %v", err)
	}
	if changed.Type != "ledger_changed" {
		t.Fatalf("changed message = %#v, want ledger_changed", changed)
	}
}

func TestProjectLedgerWebSocketNotificationsStayProjectScoped(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha", "beta")
	alpha, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project alpha: %v", err)
	}
	beta, err := manager.Project("beta")
	if err != nil {
		t.Fatalf("Project beta: %v", err)
	}
	server := httptest.NewServer(New(Deps{Config: cfg, Manager: manager, AuthDisabled: true}))
	defer server.Close()

	dialProjectLedger := func(slug string) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/"+slug+"/ledger"), nil)
		if err != nil {
			t.Fatalf("Dial %s ledger: %v", slug, err)
		}
		var ready wsMessage
		if err := conn.ReadJSON(&ready); err != nil {
			_ = conn.Close()
			t.Fatalf("ReadJSON %s ready: %v", slug, err)
		}
		if ready.Type != "ready" {
			_ = conn.Close()
			t.Fatalf("%s ready message = %#v, want ready", slug, ready)
		}
		return conn
	}
	assertLedgerChanged := func(conn *websocket.Conn, label string) {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("%s SetReadDeadline: %v", label, err)
		}
		defer func() {
			_ = conn.SetReadDeadline(time.Time{})
		}()
		var changed wsMessage
		if err := conn.ReadJSON(&changed); err != nil {
			t.Fatalf("%s ReadJSON changed: %v", label, err)
		}
		if changed.Type != "ledger_changed" {
			t.Fatalf("%s message = %#v, want ledger_changed", label, changed)
		}
	}
	assertNoLedgerChanged := func(conn *websocket.Conn, label string) {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatalf("%s SetReadDeadline: %v", label, err)
		}
		var msg wsMessage
		err := conn.ReadJSON(&msg)
		if err == nil {
			t.Fatalf("%s received %#v, want no message", label, msg)
		}
		var timeoutErr interface{ Timeout() bool }
		if !errors.As(err, &timeoutErr) || !timeoutErr.Timeout() {
			t.Fatalf("%s ReadJSON = %v, want timeout", label, err)
		}
	}

	alphaConn := dialProjectLedger("alpha")
	betaConn := dialProjectLedger("beta")
	if err := alpha.Ledger.Add(ledger.Task{ID: "task-20260101-0001", Title: "Alpha change", Status: "unstarted"}); err != nil {
		t.Fatalf("Add alpha: %v", err)
	}
	assertLedgerChanged(alphaConn, "alpha subscriber after alpha change")
	assertNoLedgerChanged(betaConn, "beta subscriber after alpha change")
	_ = alphaConn.Close()
	_ = betaConn.Close()

	alphaConn = dialProjectLedger("alpha")
	betaConn = dialProjectLedger("beta")
	if err := beta.Ledger.Add(ledger.Task{ID: "task-20260101-0002", Title: "Beta change", Status: "unstarted"}); err != nil {
		t.Fatalf("Add beta: %v", err)
	}
	assertLedgerChanged(betaConn, "beta subscriber after beta change")
	assertNoLedgerChanged(alphaConn, "alpha subscriber after beta change")
	_ = alphaConn.Close()
	_ = betaConn.Close()
}

func TestProjectPostTaskAcceptsNaturalLanguageRequest(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Runtime = config.RuntimeConfig{TmuxSession: "ccx-test", WorktreeBase: filepath.Join(dir, "worktrees")}
	cfg.Orchestrator.Timeout = 0
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
	handler := New(Deps{Config: cfg, Manager: manager, AuthDisabled: true})

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/tasks", `{
		"request": "Turn this plain request into researched implementation tasks."
	}`)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusAccepted, resp.Body.String())
	}
	var created taskCreateResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if created.Task.Title != "Natural language intake" || !strings.Contains(created.Task.Body, "plain request") {
		t.Fatalf("created task = %#v, want natural-language intake with raw request", created.Task)
	}
	if len(created.Task.AllowedFiles) != 0 {
		t.Fatalf("allowed_files = %#v, want none for raw intake", created.Task.AllowedFiles)
	}

	alphaTasks, err := alpha.Ledger.Load()
	if err != nil {
		t.Fatalf("Load alpha: %v", err)
	}
	if len(alphaTasks) != 1 || alphaTasks[0].Title != "Natural language intake" || !strings.Contains(alphaTasks[0].Body, "plain request") {
		t.Fatalf("alpha tasks = %#v, want raw intake in selected project", alphaTasks)
	}
	betaTasks, err := beta.Ledger.Load()
	if err != nil {
		t.Fatalf("Load beta: %v", err)
	}
	if len(betaTasks) != 0 {
		t.Fatalf("beta tasks = %#v, want untouched beta ledger", betaTasks)
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

func TestPostTasksAcceptsNaturalLanguageRequestWithoutStructuredFields(t *testing.T) {
	l := newTestLedger(t)
	trigger := &fakeTrigger{}
	server := New(Deps{Ledger: l, Trigger: trigger, AuthDisabled: true})

	resp := performJSONRequest(server, http.MethodPost, "/api/tasks", `{
		"body": "\n",
		"request": "Please figure out how task intake should work, then create the right implementation tasks."
	}`)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}

	var created taskCreateResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if created.Task.Title != "Natural language intake" {
		t.Fatalf("title = %q, want natural-language intake title", created.Task.Title)
	}
	if !strings.Contains(created.Task.Body, "Please figure out how task intake should work") {
		t.Fatalf("body = %q, want raw request preserved", created.Task.Body)
	}
	if len(created.Task.AllowedFiles) != 0 {
		t.Fatalf("allowed_files = %#v, want none for raw intake", created.Task.AllowedFiles)
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

func TestPostTasksDuplicateIdempotencyKeyDoesNotRetriggerAfterSuccess(t *testing.T) {
	l := newTestLedger(t)
	trigger := &fakeTrigger{}
	server := New(Deps{Ledger: l, Trigger: trigger, AuthDisabled: true})
	body := `{"idempotency_key":"task-20260101-0001","title":"New task"}`

	first := performJSONRequest(server, http.MethodPost, "/api/tasks", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want %d; body=%s", first.Code, http.StatusCreated, first.Body.String())
	}
	second := performJSONRequest(server, http.MethodPost, "/api/tasks", body)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d; body=%s", second.Code, http.StatusOK, second.Body.String())
	}
	if len(trigger.reasons) != 1 {
		t.Fatalf("trigger count = %d, want 1", len(trigger.reasons))
	}
	var replay taskCreateResponse
	if err := json.Unmarshal(second.Body.Bytes(), &replay); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if replay.OrchestratorTriggered {
		t.Fatal("replay OrchestratorTriggered = true, want false for already-triggered task")
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

func TestPostTasksRejectsUnknownStatus(t *testing.T) {
	l := newTestLedger(t)
	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPost, "/api/tasks", `{
		"title": "New task",
		"status": "waiting"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks = %#v, want invalid task rejected before persistence", tasks)
	}
}

func TestPostTasksRejectsEscapingAllowedFile(t *testing.T) {
	l := newTestLedger(t)
	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPost, "/api/tasks", `{
		"title": "New task",
		"allowed_files": ["../secrets.env"]
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks = %#v, want invalid task rejected before persistence", tasks)
	}
}

func TestPostTasksRejectsAbsoluteForbiddenFile(t *testing.T) {
	l := newTestLedger(t)
	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPost, "/api/tasks", `{
		"title": "New task",
		"forbidden_files": ["/etc/passwd"]
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks = %#v, want invalid task rejected before persistence", tasks)
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

func TestPostTasksIdempotentRetryAfterTriggerPanic(t *testing.T) {
	l := newTestLedger(t)
	trigger := &fakeTrigger{
		fn: func(context.Context, string) error {
			panic("trigger panic")
		},
	}
	server := New(Deps{Ledger: l, Trigger: trigger, AuthDisabled: true})
	body := `{"idempotency_key":"task-20260101-0001","title":"New task"}`

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("first request panic = nil, want trigger panic")
			}
		}()
		_ = performJSONRequest(server, http.MethodPost, "/api/tasks", body)
	}()

	trigger.fn = nil
	retry := performJSONRequest(server, http.MethodPost, "/api/tasks", body)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want %d; body=%s", retry.Code, http.StatusOK, retry.Body.String())
	}
	if len(trigger.reasons) != 2 {
		t.Fatalf("trigger count = %d, want 2", len(trigger.reasons))
	}
	var created taskCreateResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if !created.OrchestratorTriggered {
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

func TestPatchTaskIgnoresEmptyNaturalLanguageRequest(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted", Body: "old body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{"request":" "}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Body != "old body" {
		t.Fatalf("tasks = %#v, want body preserved", tasks)
	}
}

func TestPatchTaskUpdatesBodyFromNaturalLanguageRequest(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted", Body: "old body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{
		"request": "updated via request field"
	}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var updated taskResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated task: %v", err)
	}
	if updated.Body != "updated via request field" {
		t.Fatalf("body = %q, want request text", updated.Body)
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

func TestPatchRejectsUnknownStatus(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{"status": "waiting"}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Status != "unstarted" {
		t.Fatalf("tasks = %#v, want status unchanged", tasks)
	}
}

func TestPatchRejectsAbsoluteForbiddenFile(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{"forbidden_files": ["/etc/passwd"]}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || len(tasks[0].ForbiddenFiles) != 0 {
		t.Fatalf("tasks = %#v, want forbidden_files unchanged", tasks)
	}
}

func TestPatchAllowsUnchangedLegacyInvalidPaths(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{
		ID:           "task-001",
		Title:        "Old",
		Status:       "unstarted",
		AllowedFiles: []string{"../legacy"},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{"title": "Updated"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "Updated" || len(tasks[0].AllowedFiles) != 1 || tasks[0].AllowedFiles[0] != "../legacy" {
		t.Fatalf("tasks = %#v, want title update without rewriting legacy paths", tasks)
	}
}

func TestPatchAcceptsValidTaskStatusAndPaths(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Old", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resp := performJSONRequest(New(Deps{Ledger: l, AuthDisabled: true}), http.MethodPatch, "/api/tasks/task-001", `{
		"status": "split",
		"allowed_files": ["server/internal/web/server.go"],
		"forbidden_files": ["server/internal/mcp/handlers.go"]
	}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].Status != "split" {
		t.Fatalf("task status = %q, want split", tasks[0].Status)
	}
	if len(tasks[0].AllowedFiles) != 1 || tasks[0].AllowedFiles[0] != "server/internal/web/server.go" {
		t.Fatalf("allowed_files = %#v, want server/internal/web/server.go", tasks[0].AllowedFiles)
	}
	if len(tasks[0].ForbiddenFiles) != 1 || tasks[0].ForbiddenFiles[0] != "server/internal/mcp/handlers.go" {
		t.Fatalf("forbidden_files = %#v, want server/internal/mcp/handlers.go", tasks[0].ForbiddenFiles)
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

func TestDeleteInProgressTaskCleanupUsesBoundedContextWithoutRequestCancel(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{
		ID:       "task-001",
		Title:    "Delete worker",
		Status:   "in_progress",
		WorkerID: "worker-task-001",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cleaner := &fakeCleaner{
		fnCtx: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("cleanup context already canceled: %w", err)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				return errors.New("cleanup context has no deadline")
			}
			now := time.Now()
			if deadline.Before(now) || deadline.After(now.Add(deleteCleanupTimeout+time.Second)) {
				return fmt.Errorf("cleanup deadline = %v, want within delete cleanup timeout", deadline)
			}
			if !deadline.Before(now.Add(deleteCleanupLease)) {
				return fmt.Errorf("cleanup deadline = %v, want shorter than delete cleanup lease", deadline)
			}
			return nil
		},
	}
	handler := New(Deps{Ledger: l, Cleaner: cleaner, AuthDisabled: true})

	req := httptest.NewRequest(http.MethodDelete, "/api/tasks/task-001", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if len(cleaner.tasks) != 1 {
		t.Fatalf("cleanup calls = %d, want 1", len(cleaner.tasks))
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

func TestDefaultWorkerCleanerSkipsUnsafeDefaultBranchDelete(t *testing.T) {
	repoPath := initWebTestRepo(t)
	cfg := testConfig()
	cfg.Project.RepoPath = repoPath
	cfg.Project.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	cleaner := defaultWorkerCleaner{
		deps: func() cleanupDependencies {
			return cleanupDependencies{cfg: cfg}
		},
		registry: worker.NewRegistry(),
	}

	err := cleaner.CleanupWorker(context.Background(), ledger.Task{
		ID:     "task-001",
		Status: "in_progress",
		Branch: "main",
	})
	if err != nil {
		t.Fatalf("CleanupWorker: %v", err)
	}
	if !webTestBranchExists(t, repoPath, "main") {
		t.Fatal("main was deleted, want preserved")
	}
}

func TestDefaultWorkerCleanerSkipsOriginUnavailableBranchDelete(t *testing.T) {
	repoPath := initWebTestRepo(t)
	runWebGit(t, repoPath, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
	runWebGit(t, repoPath, "branch", "feature/task-001")
	cfg := testConfig()
	cfg.Project.RepoPath = repoPath
	cfg.Project.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-task-001", TaskID: "task-001"})
	cleaner := defaultWorkerCleaner{
		deps: func() cleanupDependencies {
			return cleanupDependencies{cfg: cfg}
		},
		registry: registry,
	}

	err := cleaner.CleanupWorker(context.Background(), ledger.Task{
		ID:       "task-001",
		Status:   "in_progress",
		WorkerID: "worker-task-001",
		Branch:   "feature/task-001",
	})
	if err != nil {
		t.Fatalf("CleanupWorker: %v", err)
	}
	if !webTestBranchExists(t, repoPath, "feature/task-001") {
		t.Fatal("feature/task-001 was deleted, want preserved when origin is unavailable")
	}
	if _, ok := registry.Get("worker-task-001"); ok {
		t.Fatal("worker registry entry still exists, want cleanup to continue")
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
	if cfgResp.Server.Host != "127.0.0.1" {
		t.Fatalf("server.host = %q, want 127.0.0.1", cfgResp.Server.Host)
	}
	if cfgResp.GitHub.Owner != "xpadev-net" || cfgResp.GitHub.Repo != "ccx-t2" {
		t.Fatalf("github = %#v, want owner/repo", cfgResp.GitHub)
	}
	if got := cfgResp.Harnesses["worker"].Command; got != "sh" {
		t.Fatalf("worker harness command = %q, want sh", got)
	}
}

func TestGetConfigReturnsEmptyWorkerHarnessesArray(t *testing.T) {
	cfg := testConfig()
	cfg.WorkerHarnesses = nil

	resp := performRequest(New(Deps{Config: cfg, AuthDisabled: true}), http.MethodGet, "/api/config")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"worker_harnesses":[]`) {
		t.Fatalf("config response = %s, want worker_harnesses empty array", resp.Body.String())
	}
}

func TestGetHarnessesReturnsEmptyArray(t *testing.T) {
	cfg := testConfig()
	cfg.WorkerHarnesses = nil

	resp := performRequest(New(Deps{Config: cfg, AuthDisabled: true}), http.MethodGet, "/api/harnesses")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := strings.TrimSpace(resp.Body.String()); got != "[]" {
		t.Fatalf("harnesses response = %s, want []", got)
	}
}

func TestApplyConfigPatchPreservesExplicitEmptyWorkerHarnesses(t *testing.T) {
	cfg := testConfig()
	empty := []string{}

	if err := applyConfigPatch(cfg, configPatchRequest{WorkerHarnesses: &empty}); err != nil {
		t.Fatalf("applyConfigPatch: %v", err)
	}
	if cfg.WorkerHarnesses == nil {
		t.Fatal("WorkerHarnesses = nil, want explicit empty slice")
	}
	if len(cfg.WorkerHarnesses) != 0 {
		t.Fatalf("WorkerHarnesses = %#v, want empty", cfg.WorkerHarnesses)
	}
}

func TestPatchConfigUpdatesAndPersistsEditableFields(t *testing.T) {
	t.Setenv("CCX_TEST_SECRET", "secret-value")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	makeWebConfigPathsValid(t, cfg)
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

	nextRepo := initWebTestRepo(t)
	resp := performJSONRequest(server, http.MethodPatch, "/api/config", `{
		"project": {"slug": "next", "repo_path": "`+nextRepo+`"},
		"server": {"host": "0.0.0.0", "port": 9091},
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
	if patched.Project.Slug != "next" || patched.Project.RepoPath != canonicalWebTestPath(t, nextRepo) || patched.Server.Host != "0.0.0.0" || patched.Server.Port != 9091 {
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
	makeWebConfigPathsValid(t, cfg)
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

func TestPatchConfigRejectsInvalidProjectSlug(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{
			name: "top level whitespace",
			body: map[string]any{"project": map[string]any{"slug": "alpha beta"}},
		},
		{
			name: "project map slash",
			body: map[string]any{"projects": map[string]any{"alpha/beta": map[string]any{"repo_path": "/unused"}}},
		},
		{
			name: "project rename parent traversal",
			body: map[string]any{"projects": map[string]any{"ccx-t2": map[string]any{"slug": ".."}}},
		},
		{
			name: "project rename control character",
			body: map[string]any{"projects": map[string]any{"ccx-t2": map[string]any{"slug": "alpha\nbeta"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			cfg := testConfig()
			makeWebConfigPathsValid(t, cfg)
			cfg.Projects = map[string]config.ProjectConfig{cfg.Project.Slug: cfg.Project}
			if err := config.Save(configPath, cfg); err != nil {
				t.Fatalf("Save config: %v", err)
			}
			loaded, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("Load config: %v", err)
			}
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("Marshal body: %v", err)
			}

			resp := performJSONRequest(New(Deps{Config: loaded, ConfigPath: configPath, AuthDisabled: true}), http.MethodPatch, "/api/config", string(body))
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
			}
			if !strings.Contains(resp.Body.String(), "slug") || !strings.Contains(resp.Body.String(), "must match") {
				t.Fatalf("body = %s, want project slug guidance", resp.Body.String())
			}
		})
	}
}

func TestPatchConfigRejectsUnsafeProjectLedgerPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	makeWebConfigPathsValid(t, cfg)
	cfg.Projects = map[string]config.ProjectConfig{
		cfg.Project.Slug: cfg.Project,
	}
	cfg.Project = config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, AuthDisabled: true})
	unsafeLedger := filepath.Join(t.TempDir(), "ledger.md")

	resp := performJSONRequest(server, http.MethodPatch, "/api/config", `{
		"projects": {"ccx-t2": {"ledger_path": "`+unsafeLedger+`"}}
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ledger_path") || !strings.Contains(resp.Body.String(), "under repo_path") {
		t.Fatalf("body = %s, want ledger containment guidance", resp.Body.String())
	}
}

func TestPatchConfigRejectsUnsafeTopLevelProjectLedgerPathWithProjects(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	makeWebConfigPathsValid(t, cfg)
	cfg.Projects = map[string]config.ProjectConfig{
		cfg.Project.Slug: cfg.Project,
	}
	cfg.Project = config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, AuthDisabled: true})
	repoPath := initWebTestRepo(t)
	worktreeBase := mkdirWebTestDir(t, "top-level-worktrees")
	unsafeLedger := filepath.Join(t.TempDir(), "ledger.md")

	resp := performJSONRequest(server, http.MethodPatch, "/api/config", `{
		"project": {
			"slug": "rogue",
			"repo_path": "`+repoPath+`",
			"worktree_base": "`+worktreeBase+`",
			"ledger_path": "`+unsafeLedger+`"
		}
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "project.ledger_path") {
		t.Fatalf("body = %s, want top-level project ledger_path guidance", resp.Body.String())
	}
}

func TestPatchConfigRejectsRelativeRuntimeWorktreeBase(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	makeWebConfigPathsValid(t, cfg)
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, AuthDisabled: true})

	resp := performJSONRequest(server, http.MethodPatch, "/api/config", `{
		"runtime": {"worktree_base": "relative-worktrees"}
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "runtime.worktree_base") || !strings.Contains(resp.Body.String(), "absolute") {
		t.Fatalf("body = %s, want runtime worktree_base guidance", resp.Body.String())
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

func TestCreateProjectPersistsAndReloadsManager(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Runtime.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	cfg.Project = config.ProjectConfig{}
	cfg.Projects = map[string]config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	manager, err := runtimepkg.NewManager(loaded, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, Manager: manager, AuthDisabled: true})

	repoPath := initWebTestRepo(t)
	resp := performJSONRequest(server, http.MethodPost, "/api/projects", `{
		"slug": "alpha",
		"repo_path": "`+repoPath+`"
	}`)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusCreated, resp.Body.String())
	}
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load reloaded config: %v", err)
	}
	if _, ok := reloaded.Projects["alpha"]; !ok {
		t.Fatalf("project alpha missing from saved config: %#v", reloaded.Projects)
	}
	if got, want := reloaded.Projects["alpha"].LedgerPath, filepath.Join(canonicalWebTestPath(t, repoPath), "tasks", "ledger.md"); got != want {
		t.Fatalf("project alpha ledger_path = %q, want %q", got, want)
	}
	tasks := performRequest(server, http.MethodGet, "/api/projects/alpha/tasks")
	if tasks.Code != http.StatusOK {
		t.Fatalf("project tasks status = %d, want %d; body=%s", tasks.Code, http.StatusOK, tasks.Body.String())
	}
}

func TestCreateProjectRejectsInvalidSlug(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Runtime.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	cfg.Project = config.ProjectConfig{}
	cfg.Projects = map[string]config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, AuthDisabled: true})
	repoPath := initWebTestRepo(t)

	for _, slug := range []string{"alpha/beta", "..", "alpha beta", "alpha\nbeta"} {
		t.Run(strconv.Quote(slug), func(t *testing.T) {
			body, err := json.Marshal(map[string]string{
				"slug":      slug,
				"repo_path": repoPath,
			})
			if err != nil {
				t.Fatalf("Marshal body: %v", err)
			}

			resp := performJSONRequest(server, http.MethodPost, "/api/projects", string(body))
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
			}
			if !strings.Contains(resp.Body.String(), "slug") || !strings.Contains(resp.Body.String(), "must match") {
				t.Fatalf("body = %s, want project slug guidance", resp.Body.String())
			}
		})
	}
}

func TestCreateProjectRejectsInvalidRepoPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Runtime.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	cfg.Project = config.ProjectConfig{}
	cfg.Projects = map[string]config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	manager, err := runtimepkg.NewManager(loaded, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, Manager: manager, AuthDisabled: true})
	nonRepo := mkdirWebTestDir(t, "not-git")

	resp := performJSONRequest(server, http.MethodPost, "/api/projects", `{
		"slug": "alpha",
		"repo_path": "`+nonRepo+`"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "repo_path") {
		t.Fatalf("body = %s, want repo_path guidance", resp.Body.String())
	}
}

func TestCreateProjectRejectsUnsafeLedgerPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Runtime.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	cfg.Project = config.ProjectConfig{}
	cfg.Projects = map[string]config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	manager, err := runtimepkg.NewManager(loaded, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, Manager: manager, AuthDisabled: true})
	repoPath := initWebTestRepo(t)
	unsafeLedger := filepath.Join(t.TempDir(), "ledger.md")

	resp := performJSONRequest(server, http.MethodPost, "/api/projects", `{
		"slug": "alpha",
		"repo_path": "`+repoPath+`",
		"ledger_path": "`+unsafeLedger+`"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ledger_path") || !strings.Contains(resp.Body.String(), "under repo_path") {
		t.Fatalf("body = %s, want ledger containment guidance", resp.Body.String())
	}
}

func TestCreateProjectRejectsRepoInternalLedgerPathOutsideDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Runtime.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	cfg.Project = config.ProjectConfig{}
	cfg.Projects = map[string]config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	manager, err := runtimepkg.NewManager(loaded, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, Manager: manager, AuthDisabled: true})
	repoPath := initWebTestRepo(t)
	repoConfig := filepath.Join(repoPath, ".git", "config")

	resp := performJSONRequest(server, http.MethodPost, "/api/projects", `{
		"slug": "alpha",
		"repo_path": "`+repoPath+`",
		"ledger_path": "`+repoConfig+`"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ledger_path") || !strings.Contains(resp.Body.String(), "default repo ledger") {
		t.Fatalf("body = %s, want default ledger guidance", resp.Body.String())
	}
}

func TestCreateProjectRejectsLedgerPathSymlinkToRepoInternalFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Runtime.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	cfg.Project = config.ProjectConfig{}
	cfg.Projects = map[string]config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	manager, err := runtimepkg.NewManager(loaded, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, Manager: manager, AuthDisabled: true})
	repoPath := initWebTestRepo(t)
	tasksDir := filepath.Join(repoPath, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll tasks: %v", err)
	}
	ledgerPath := filepath.Join(tasksDir, "ledger.md")
	if err := os.Symlink(filepath.Join(repoPath, ".git", "config"), ledgerPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	resp := performJSONRequest(server, http.MethodPost, "/api/projects", `{
		"slug": "alpha",
		"repo_path": "`+repoPath+`",
		"ledger_path": "`+ledgerPath+`"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ledger_path") || !strings.Contains(resp.Body.String(), "symlink") {
		t.Fatalf("body = %s, want ledger symlink guidance", resp.Body.String())
	}
}

func TestCreateProjectRejectsUnsafeWorktreeBase(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Runtime.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	cfg.Project = config.ProjectConfig{}
	cfg.Projects = map[string]config.ProjectConfig{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	manager, err := runtimepkg.NewManager(loaded, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, Manager: manager, AuthDisabled: true})
	repoPath := initWebTestRepo(t)

	resp := performJSONRequest(server, http.MethodPost, "/api/projects", `{
		"slug": "alpha",
		"repo_path": "`+repoPath+`",
		"worktree_base": "relative-worktrees"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "worktree_base") || !strings.Contains(resp.Body.String(), "absolute") {
		t.Fatalf("body = %s, want worktree_base guidance", resp.Body.String())
	}
}

func TestDeleteProjectPersistsAndReloadsManager(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Runtime.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	repoPath := initWebTestRepo(t)
	cfg.Project = config.ProjectConfig{}
	cfg.Projects = map[string]config.ProjectConfig{
		"alpha": {
			Slug:         "alpha",
			RepoPath:     repoPath,
			WorktreeBase: cfg.Runtime.WorktreeBase,
			LedgerPath:   filepath.Join(repoPath, "tasks", "ledger.md"),
			Orchestrator: cfg.Orchestrator,
			GitHub:       cfg.GitHub,
		},
	}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	manager, err := runtimepkg.NewManager(loaded, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := New(Deps{Config: loaded, ConfigPath: configPath, Manager: manager, AuthDisabled: true})

	resp := performRequest(server, http.MethodDelete, "/api/projects/alpha")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load reloaded config: %v", err)
	}
	if _, ok := reloaded.Projects["alpha"]; ok {
		t.Fatalf("project alpha still present in saved config: %#v", reloaded.Projects)
	}
	tasks := performRequest(server, http.MethodGet, "/api/projects/alpha/tasks")
	if tasks.Code != http.StatusNotFound {
		t.Fatalf("project tasks status = %d, want %d; body=%s", tasks.Code, http.StatusNotFound, tasks.Body.String())
	}
}

func TestPatchConfigConcurrentRequestsPreserveUpdates(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	makeWebConfigPathsValid(t, cfg)
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
	makeWebConfigPathsValid(t, cfg)
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
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-task-001", TaskID: "task-001"})
	server := httptest.NewServer(New(Deps{
		Config:   testConfig(),
		Registry: registry,
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
		if msg.Type != "chunk" || msg.Data != want {
			t.Fatalf("message = %#v, want chunk %q", msg, want)
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
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-task-001", TaskID: "task-001"})
	server := httptest.NewServer(New(Deps{
		Config:   testConfig(),
		Registry: registry,
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

func TestProjectWorkerLogWebSocketStreamsProjectWorker(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	alpha, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project alpha: %v", err)
	}
	if err := alpha.Ledger.Add(ledger.Task{
		ID:       "task-20260101-0001",
		Title:    "Active worker",
		Status:   "in_progress",
		WorkerID: "alpha-worker-task-20260101-0001",
	}); err != nil {
		t.Fatalf("Add alpha task: %v", err)
	}
	lines := make(chan string, 1)
	lines <- "alpha ready"
	close(lines)
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			if session != "ccx-test" || window != "alpha-worker-task-20260101-0001" {
				t.Fatalf("pipe args = %q %q, want ccx-test alpha-worker-task-20260101-0001", session, window)
			}
			return lines, func() {}, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/worker/alpha-worker-task-20260101-0001"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var msg wsMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if msg.Type != "chunk" || msg.Data != "alpha ready" {
		t.Fatalf("message = %#v, want alpha worker chunk", msg)
	}
}

func TestWorkerLogWebSocketStreamsSelectedProjectWorker(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	alpha, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project alpha: %v", err)
	}
	if err := alpha.Ledger.Add(ledger.Task{
		ID:       "task-20260101-0001",
		Title:    "Active worker",
		Status:   "in_progress",
		WorkerID: "alpha-worker-task-20260101-0001",
	}); err != nil {
		t.Fatalf("Add alpha task: %v", err)
	}
	lines := make(chan string, 1)
	lines <- "alpha root ready"
	close(lines)
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			if session != "ccx-test" || window != "alpha-worker-task-20260101-0001" {
				t.Fatalf("pipe args = %q %q, want ccx-test alpha-worker-task-20260101-0001", session, window)
			}
			return lines, func() {}, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/worker/alpha-worker-task-20260101-0001"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var msg wsMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if msg.Type != "chunk" || msg.Data != "alpha root ready" {
		t.Fatalf("message = %#v, want selected project worker chunk", msg)
	}
}

func TestWorkerLogWebSocketReportsSelectedProjectResolutionFailure(t *testing.T) {
	cfg, _ := newTestProjectManager(t, "alpha")
	_, manager := newTestProjectManager(t, "beta")
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			t.Fatalf("PipeOutput called when selected project could not be resolved")
			return nil, nil, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/worker/alpha-worker-task-20260101-0001"), nil)
	if err == nil {
		t.Fatal("Dial error = nil, want selected project failure")
	}
	if resp == nil || resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("response = %#v, want 500", resp)
	}
}

func TestAuthorizeWorkerLogWindowFailsClosedWithoutProjectPrefix(t *testing.T) {
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-task-001", TaskID: "task-001"})
	server := &Server{projectScoped: true, registry: registry}

	status, message := server.authorizeWorkerLogWindow("worker-task-001")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d message = %q, want 403", status, message)
	}
}

func TestProjectWorkerLogWebSocketRejectsCrossProjectWorker(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha", "beta")
	beta, err := manager.Project("beta")
	if err != nil {
		t.Fatalf("Project beta: %v", err)
	}
	if err := beta.Ledger.Add(ledger.Task{
		ID:       "task-20260101-0002",
		Title:    "Beta worker",
		Status:   "in_progress",
		WorkerID: "beta-worker-task-20260101-0002",
	}); err != nil {
		t.Fatalf("Add beta task: %v", err)
	}
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			t.Fatalf("PipeOutput called for unauthorized worker %q in session %q", window, session)
			return nil, nil, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/worker/beta-worker-task-20260101-0002"), nil)
	if err == nil {
		t.Fatal("Dial error = nil, want forbidden")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %#v, want 403", resp)
	}
}

func TestProjectWorkerLogWebSocketRejectsArbitraryTmuxWindow(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			t.Fatalf("PipeOutput called for unauthorized window %q in session %q", window, session)
			return nil, nil, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/worker/shell"), nil)
	if err == nil {
		t.Fatal("Dial error = nil, want forbidden")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %#v, want 403", resp)
	}
}

func TestProjectWorkerLogWebSocketRejectsLedgerOwnedWindowWithoutProjectPrefix(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	alpha, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project alpha: %v", err)
	}
	if err := alpha.Ledger.Add(ledger.Task{
		ID:       "task-20260101-0001",
		Title:    "Unsafe worker",
		Status:   "in_progress",
		WorkerID: "shell",
	}); err != nil {
		t.Fatalf("Add alpha task: %v", err)
	}
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			t.Fatalf("PipeOutput called for ledger-owned non-project window %q in session %q", window, session)
			return nil, nil, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/worker/shell"), nil)
	if err == nil {
		t.Fatal("Dial error = nil, want forbidden")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %#v, want 403", resp)
	}
}

func TestProjectWorkerLogWebSocketRejectsMissingWorker(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			t.Fatalf("PipeOutput called for missing worker %q in session %q", window, session)
			return nil, nil, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/worker/alpha-worker-task-404"), nil)
	if err == nil {
		t.Fatal("Dial error = nil, want not found")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("response = %#v, want 404", resp)
	}
}

func TestProjectOrchestratorLogWebSocketStreamsProjectWindow(t *testing.T) {
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
	}
	manager, err := runtimepkg.NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	lines := make(chan string, 1)
	lines <- "orchestrator ready"
	close(lines)
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			if session != "ccx-test" {
				t.Fatalf("session args = %q, want ccx-test", session)
			}
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("alive args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return true, nil
		},
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("pipe args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return lines, func() {}, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/orchestrator"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var msg wsMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if msg.Type != "chunk" || msg.Data != "orchestrator ready" {
		t.Fatalf("message = %#v, want orchestrator chunk", msg)
	}
}

func TestProjectOrchestratorLogWebSocketSendsInitialPaneSnapshot(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	lines := make(chan []byte)
	defer close(lines)
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			return true, nil
		},
		PipeBytes: func(session, window string) (<-chan []byte, func(), error) {
			return lines, func() {}, nil
		},
		CapturePane: func(ctx context.Context, session, window string) ([]byte, error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("capture args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return []byte("existing shell\r\n$ "), nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/orchestrator"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var msg wsMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if msg.Type != "chunk" || msg.Data != "existing shell\r\n$ " {
		t.Fatalf("message = %#v, want initial shell snapshot", msg)
	}
}

func TestProjectOrchestratorLogWebSocketForwardsTerminalInput(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	lines := make(chan string)
	sent := make(chan string, 1)
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			return true, nil
		},
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("pipe args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return lines, func() {}, nil
		},
		SendRawKeys: func(ctx context.Context, session, window, keys string) error {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("send args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			sent <- keys
			return nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/orchestrator"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(wsMessage{Type: "input", Data: "\x1b[B"}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	select {
	case got := <-sent:
		if got != "\x1b[B" {
			t.Fatalf("sent keys = %q, want arrow-down escape", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal input was not forwarded")
	}
}

func TestProjectOrchestratorLogWebSocketForwardsResize(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	lines := make(chan string)
	resized := make(chan [2]int, 1)
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			return true, nil
		},
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			return lines, func() {}, nil
		},
		ResizePane: func(ctx context.Context, session, window string, cols, rows int) error {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("resize args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			resized <- [2]int{cols, rows}
			return nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/orchestrator"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(wsMessage{Type: "resize", Cols: 132, Rows: 31}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	select {
	case got := <-resized:
		if got != [2]int{132, 31} {
			t.Fatalf("resize = %#v, want 132x31", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal resize was not forwarded")
	}
	close(lines)
}

func TestProjectOrchestratorLogWebSocketStartsWhenPaneIdle(t *testing.T) {
	idle := true
	trigger := &fakeTrigger{fn: func(ctx context.Context, reason string) error {
		idle = false
		return nil
	}}
	server := New(Deps{
		Config:  testConfig(),
		Trigger: trigger,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			return true, nil
		},
		IsPaneIdle: func(ctx context.Context, session, window string) (bool, error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("idle args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return idle, nil
		},
		AuthDisabled: true,
	})

	if err := server.ensureOrchestratorAttached(context.Background(), "ccx-test", "alpha-orchestrator"); err != nil {
		t.Fatalf("ensureOrchestratorAttached: %v", err)
	}
	if len(trigger.reasons) != 1 {
		t.Fatalf("trigger count = %d, want 1", len(trigger.reasons))
	}
}

func TestProjectOrchestratorLogWebSocketStartsWhenSessionMissing(t *testing.T) {
	sessionStarted := false
	windowStarted := false
	trigger := &fakeTrigger{fn: func(ctx context.Context, reason string) error {
		if reason != "browser orchestrator web shell opened" {
			t.Fatalf("trigger reason = %q, want browser web shell reason", reason)
		}
		sessionStarted = true
		windowStarted = true
		return nil
	}}
	server := New(Deps{
		Config:  testConfig(),
		Trigger: trigger,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			if session != "ccx-test" {
				t.Fatalf("session args = %q, want ccx-test", session)
			}
			return sessionStarted, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("alive args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return windowStarted, nil
		},
		AuthDisabled: true,
	})

	if err := server.ensureOrchestratorAttached(context.Background(), "ccx-test", "alpha-orchestrator"); err != nil {
		t.Fatalf("ensureOrchestratorAttached: %v", err)
	}
	if len(trigger.reasons) != 1 {
		t.Fatalf("trigger count = %d, want 1", len(trigger.reasons))
	}
}

func TestProjectOrchestratorLogWebSocketStartsWhenWindowMissing(t *testing.T) {
	windowStarted := false
	trigger := &fakeTrigger{fn: func(ctx context.Context, reason string) error {
		windowStarted = true
		return nil
	}}
	server := New(Deps{
		Config:  testConfig(),
		Trigger: trigger,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("alive args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return windowStarted, nil
		},
		AuthDisabled: true,
	})

	if err := server.ensureOrchestratorAttached(context.Background(), "ccx-test", "alpha-orchestrator"); err != nil {
		t.Fatalf("ensureOrchestratorAttached: %v", err)
	}
	if len(trigger.reasons) != 1 {
		t.Fatalf("trigger count = %d, want 1", len(trigger.reasons))
	}
}

func TestProjectServerUsesProjectOrchestratorWhenParentTriggerConfigured(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	parentTrigger := &fakeTrigger{}
	server := New(Deps{Config: cfg, Manager: manager, Trigger: parentTrigger, AuthDisabled: true})
	projectServer, err := server.projectServer("alpha")
	if err != nil {
		t.Fatalf("projectServer: %v", err)
	}
	project, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project alpha: %v", err)
	}
	if projectServer.trigger != project.Orchestrator {
		t.Fatalf("project trigger = %#v, want project orchestrator %#v", projectServer.trigger, project.Orchestrator)
	}
}

func TestEnsureOrchestratorAttachedCoalescesConcurrentStarts(t *testing.T) {
	var mu sync.Mutex
	windowStarted := false
	paneIdle := true
	triggerCount := 0
	server := New(Deps{
		Config: testConfig(),
		Trigger: triggerFunc(func(ctx context.Context, reason string) error {
			mu.Lock()
			triggerCount++
			windowStarted = true
			mu.Unlock()
			go func() {
				time.Sleep(50 * time.Millisecond)
				mu.Lock()
				paneIdle = false
				mu.Unlock()
			}()
			return nil
		}),
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return windowStarted, nil
		},
		IsPaneIdle: func(ctx context.Context, session, window string) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return paneIdle, nil
		},
		AuthDisabled: true,
	})

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- server.ensureOrchestratorAttached(context.Background(), "ccx-test", "alpha-orchestrator")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ensureOrchestratorAttached: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if triggerCount != 1 {
		t.Fatalf("trigger count = %d, want 1", triggerCount)
	}
}

func TestProjectOrchestratorLogWebSocketAllowsMultipleSubscribers(t *testing.T) {
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
	}
	manager, err := runtimepkg.NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	lines := make(chan string)
	server := httptest.NewServer(New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			return true, nil
		},
		PipeOutput: func(session, window string) (<-chan string, func(), error) {
			return lines, func() {}, nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()
	first, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/orchestrator"), nil)
	if err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	defer first.Close()

	second, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/projects/alpha/orchestrator"), nil)
	if err != nil {
		t.Fatalf("second Dial: %v", err)
	}
	defer second.Close()

	lines <- "shared output"
	for _, conn := range []*websocket.Conn{first, second} {
		var msg wsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("ReadJSON: %v", err)
		}
		if msg.Type != "chunk" || msg.Data != "shared output" {
			t.Fatalf("message = %#v, want shared chunk", msg)
		}
	}
	close(lines)
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

func TestWebSocketAllowsEmptyOrigin(t *testing.T) {
	server := httptest.NewServer(New(Deps{
		Config:       testConfig(),
		AuthDisabled: true,
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/ledger"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
}

func TestWebSocketAllowsSameOrigin(t *testing.T) {
	server := httptest.NewServer(New(Deps{
		Config:       testConfig(),
		AuthDisabled: true,
	}))
	defer server.Close()
	header := http.Header{"Origin": []string{server.URL}}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/ledger"), header)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
}

func TestWebSocketAllowsSameOriginBehindTLSProxy(t *testing.T) {
	server := httptest.NewServer(New(Deps{
		Config:       testConfig(),
		AuthDisabled: true,
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	header := http.Header{
		"Origin":            []string{"https://" + host},
		"X-Forwarded-Proto": []string{"https"},
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/ledger"), header)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
}

func TestWebSocketAllowsConfiguredOrigin(t *testing.T) {
	server := httptest.NewServer(New(Deps{
		Config:         testConfig(),
		AuthDisabled:   true,
		AllowedOrigins: []string{"http://allowed.example"},
	}))
	defer server.Close()
	header := http.Header{"Origin": []string{"http://allowed.example"}}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/ledger"), header)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
}

func TestWorkerLogWebSocketAllowsMultipleSubscribers(t *testing.T) {
	lines := make(chan string)
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-task-001", TaskID: "task-001"})
	server := httptest.NewServer(New(Deps{
		Config:   testConfig(),
		Registry: registry,
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

	second, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/worker/worker-task-001"), nil)
	if err != nil {
		t.Fatalf("second Dial: %v", err)
	}
	defer second.Close()

	lines <- "worker output"
	for _, conn := range []*websocket.Conn{first, second} {
		var msg wsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("ReadJSON: %v", err)
		}
		if msg.Type != "chunk" || msg.Data != "worker output" {
			t.Fatalf("message = %#v, want shared worker chunk", msg)
		}
	}
	close(lines)
}

func TestProjectOrchestratorInputRequiresPostAndAuth(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	handler := New(Deps{Config: cfg, Manager: manager, Secret: "web-secret"})

	unauthorized := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/orchestrator/input", `{
		"message": "status"
	}`)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	authorizedGet := performRequestWithHeaders(handler, http.MethodGet, "/api/projects/alpha/orchestrator/input", map[string]string{
		"Authorization": "Bearer web-secret",
	})
	if authorizedGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d, want %d", authorizedGet.Code, http.StatusMethodNotAllowed)
	}
	if allow := authorizedGet.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}
}

func TestProjectOrchestratorInputRejectsMissingProject(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	handler := New(Deps{Config: cfg, Manager: manager, AuthDisabled: true})

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/missing/orchestrator/input", `{
		"message": "status"
	}`)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestProjectOrchestratorInputSendsToProjectWindow(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	var sentSession, sentWindow, sentKeys string
	handler := New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			if session != "ccx-test" {
				t.Fatalf("session = %q, want ccx-test", session)
			}
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("alive args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return true, nil
		},
		SendKeys: func(ctx context.Context, session, window, keys string) error {
			sentSession, sentWindow, sentKeys = session, window, keys
			return nil
		},
		AuthDisabled: true,
	})

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/orchestrator/input", `{
		"message": "\r\nline one\r\nline two\r\n"
	}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if sentSession != "ccx-test" || sentWindow != "alpha-orchestrator" || sentKeys != "line one\nline two" {
		t.Fatalf("sent = %q %q %q, want normalized project orchestrator input", sentSession, sentWindow, sentKeys)
	}
	var input orchestratorInputResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &input); err != nil {
		t.Fatalf("decode input response: %v", err)
	}
	if !input.Sent || input.Session != "ccx-test" || input.Window != "alpha-orchestrator" {
		t.Fatalf("input = %#v, want sent project orchestrator response", input)
	}
}

func TestProjectOrchestratorInputRejectsMissingSession(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	sendCalled := false
	handler := New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return false, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			t.Fatal("IsWindowAlive was called after missing session")
			return false, nil
		},
		SendKeys: func(ctx context.Context, session, window, keys string) error {
			sendCalled = true
			return nil
		},
		AuthDisabled: true,
	})

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/orchestrator/input", `{
		"message": "status"
	}`)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
	if sendCalled {
		t.Fatal("SendKeys was called after missing session")
	}
}

func TestProjectOrchestratorInputRejectsMissingWindow(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	sendCalled := false
	handler := New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			if session != "ccx-test" || window != "alpha-orchestrator" {
				t.Fatalf("alive args = %q %q, want ccx-test alpha-orchestrator", session, window)
			}
			return false, nil
		},
		SendKeys: func(ctx context.Context, session, window, keys string) error {
			sendCalled = true
			return nil
		},
		AuthDisabled: true,
	})

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/orchestrator/input", `{
		"message": "status"
	}`)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
	if sendCalled {
		t.Fatal("SendKeys was called after missing window")
	}
}

func TestProjectOrchestratorInputReportsSendFailure(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	handler := New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			return true, nil
		},
		SendKeys: func(ctx context.Context, session, window, keys string) error {
			return errors.New("send failed")
		},
		AuthDisabled: true,
	})

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/orchestrator/input", `{
		"message": "status"
	}`)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusInternalServerError, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "send orchestrator input") {
		t.Fatalf("body = %s, want send failure message", resp.Body.String())
	}
}

func TestProjectOrchestratorInputRejectsControlCharacters(t *testing.T) {
	cfg, manager := newTestProjectManager(t, "alpha")
	sendCalled := false
	handler := New(Deps{
		Config:  cfg,
		Manager: manager,
		IsSessionAlive: func(ctx context.Context, session string) (bool, error) {
			return true, nil
		},
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			return true, nil
		},
		SendKeys: func(ctx context.Context, session, window, keys string) error {
			sendCalled = true
			return nil
		},
		AuthDisabled: true,
	})

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/orchestrator/input", `{
		"message": "please stop\u0003"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if sendCalled {
		t.Fatal("SendKeys was called for unsafe orchestrator input")
	}
}

func TestProjectWorkerFollowupSendsToActiveWorkerWindow(t *testing.T) {
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
	}
	manager, err := runtimepkg.NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	alpha, err := manager.Project("alpha")
	if err != nil {
		t.Fatalf("Project alpha: %v", err)
	}
	if err := alpha.Ledger.Add(ledger.Task{
		ID:       "task-20260101-0001",
		Title:    "Active worker",
		Status:   "in_progress",
		WorkerID: "alpha-worker-task-20260101-0001",
	}); err != nil {
		t.Fatalf("Add alpha task: %v", err)
	}
	var sentSession, sentWindow, sentKeys string
	handler := New(Deps{
		Config:  cfg,
		Manager: manager,
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			if session != "ccx-test" || window != "alpha-worker-task-20260101-0001" {
				t.Fatalf("alive args = %q %q, want ccx-test alpha-worker-task-20260101-0001", session, window)
			}
			return true, nil
		},
		SendKeys: func(ctx context.Context, session, window, keys string) error {
			sentSession, sentWindow, sentKeys = session, window, keys
			return nil
		},
		AuthDisabled: true,
	})

	resp := performJSONRequest(handler, http.MethodPost, "/api/projects/alpha/workers/alpha-worker-task-20260101-0001/followup", `{
		"message": "please continue"
	}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if sentSession != "ccx-test" || sentWindow != "alpha-worker-task-20260101-0001" || sentKeys != "please continue" {
		t.Fatalf("sent = %q %q %q, want project worker followup", sentSession, sentWindow, sentKeys)
	}
	var followup workerFollowupResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &followup); err != nil {
		t.Fatalf("decode followup response: %v", err)
	}
	if !followup.Sent || followup.Window != "alpha-worker-task-20260101-0001" || followup.TaskID != "task-20260101-0001" {
		t.Fatalf("followup = %#v, want sent project worker response", followup)
	}
}

func TestWorkerFollowupRejectsControlCharacters(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{
		ID:       "task-20260101-0001",
		Title:    "Active worker",
		Status:   "in_progress",
		WorkerID: "worker-task-20260101-0001",
	}); err != nil {
		t.Fatalf("Add task: %v", err)
	}
	sendCalled := false
	handler := New(Deps{
		Config: testConfig(),
		Ledger: l,
		IsWindowAlive: func(ctx context.Context, session, window string) (bool, error) {
			return true, nil
		},
		SendKeys: func(ctx context.Context, session, window, keys string) error {
			sendCalled = true
			return nil
		},
		AuthDisabled: true,
	})

	resp := performJSONRequest(handler, http.MethodPost, "/api/workers/worker-task-20260101-0001/followup", `{
		"message": "please stop\u0003"
	}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if sendCalled {
		t.Fatal("SendKeys was called for unsafe followup input")
	}
}

func TestNormalizeFollowupMessageAllowsCRLF(t *testing.T) {
	got, err := normalizeFollowupMessage("line one\r\nline two\r\n")
	if err != nil {
		t.Fatalf("normalizeFollowupMessage: %v", err)
	}
	if got != "line one\nline two" {
		t.Fatalf("message = %q, want normalized LF lines", got)
	}
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

func newTestProjectManager(t *testing.T, slugs ...string) (*config.Config, *runtimepkg.Manager) {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Runtime = config.RuntimeConfig{TmuxSession: "ccx-test", WorktreeBase: filepath.Join(dir, "worktrees")}
	cfg.Projects = make(map[string]config.ProjectConfig, len(slugs))
	for _, slug := range slugs {
		cfg.Projects[slug] = config.ProjectConfig{
			Slug:         slug,
			RepoPath:     filepath.Join(dir, slug),
			LedgerPath:   filepath.Join(dir, slug, "tasks", "ledger.md"),
			WorktreeBase: cfg.Runtime.WorktreeBase,
			Orchestrator: cfg.Orchestrator,
			GitHub:       cfg.GitHub,
		}
	}
	if len(slugs) > 0 {
		cfg.Project = cfg.Projects[slugs[0]]
	}
	manager, err := runtimepkg.NewManager(cfg, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return cfg, manager
}

func makeWebConfigPathsValid(t *testing.T, cfg *config.Config) {
	t.Helper()
	cfg.Project.RepoPath = initWebTestRepo(t)
	cfg.Project.WorktreeBase = mkdirWebTestDir(t, "worktrees")
	if cfg.Project.LedgerPath != "" {
		cfg.Project.LedgerPath = filepath.Join(cfg.Project.RepoPath, "tasks", "ledger.md")
	}
}

func initWebTestRepo(t *testing.T) string {
	t.Helper()
	repoPath := mkdirWebTestDir(t, "repo")
	runWebGit(t, repoPath, "init", "-b", "main")
	runWebGit(t, repoPath, "config", "user.email", "test@example.com")
	runWebGit(t, repoPath, "config", "user.name", "Test User")
	runWebGit(t, repoPath, "commit", "--allow-empty", "-m", "init")
	return repoPath
}

func mkdirWebTestDir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
	return path
}

func runWebGit(t *testing.T, repoPath string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", repoPath}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func canonicalWebTestPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs %s: %v", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatalf("EvalSymlinks %s: %v", path, err)
	}
	return resolved
}

func webTestBranchExists(t *testing.T, repoPath, branch string) bool {
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

func testConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{
			Slug:              "ccx-t2",
			RepoPath:          "/repo",
			WorktreeBase:      "/worktrees",
			ValidationCommand: "go test ./...",
		},
		Server: config.ServerConfig{
			Host: "127.0.0.1",
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
	fn      func(context.Context, string) error
}

func (f *fakeTrigger) Trigger(ctx context.Context, reason string) error {
	f.reasons = append(f.reasons, reason)
	if f.fn != nil {
		return f.fn(ctx, reason)
	}
	return f.err
}

type triggerFunc func(context.Context, string) error

func (f triggerFunc) Trigger(ctx context.Context, reason string) error {
	return f(ctx, reason)
}

type fakeCleaner struct {
	tasks    []ledger.Task
	contexts []context.Context
	err      error
	fn       func() error
	fnCtx    func(context.Context) error
}

func (f *fakeCleaner) CleanupWorker(ctx context.Context, task ledger.Task) error {
	f.tasks = append(f.tasks, task)
	f.contexts = append(f.contexts, ctx)
	if f.fnCtx != nil {
		return f.fnCtx(ctx)
	}
	if f.fn != nil {
		return f.fn()
	}
	return f.err
}
