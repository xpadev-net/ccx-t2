package event

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/xpadev/ccx-t2/internal/ledger"
)

type fakeTrigger struct {
	reasons []string
}

func (f *fakeTrigger) Trigger(ctx context.Context, reason string) error {
	f.reasons = append(f.reasons, reason)
	return nil
}

func TestQueueRunProcessesEventsFIFOAndTriggers(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "First", Status: "in_progress", WorkerID: "worker-1"}); err != nil {
		t.Fatalf("Add task-001: %v", err)
	}
	if err := l.Add(ledger.Task{ID: "task-002", Title: "Second", Status: "in_progress", WorkerID: "worker-2"}); err != nil {
		t.Fatalf("Add task-002: %v", err)
	}
	trigger := &fakeTrigger{}
	q := NewQueue(l, trigger, 2)

	ctx := context.Background()
	if err := q.Enqueue(ctx, Event{Type: Blocked, TaskID: "task-001", WorkerID: "worker-1", Reason: "needs input"}); err != nil {
		t.Fatalf("Enqueue blocked: %v", err)
	}
	if err := q.Enqueue(ctx, Event{Type: Completed, TaskID: "task-002", WorkerID: "worker-2", PRURL: "https://example.test/pr/2", MergeCommit: "abc123"}); err != nil {
		t.Fatalf("Enqueue completed: %v", err)
	}
	q.Close()
	if err := q.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	tasksByID := loadTasksByID(t, l)
	if got := tasksByID["task-001"].Status; got != "blocked" {
		t.Fatalf("task-001 status = %q, want blocked", got)
	}
	if got := tasksByID["task-001"].Reason; got != "needs input" {
		t.Fatalf("task-001 reason = %q, want needs input", got)
	}
	if got := tasksByID["task-002"].Status; got != "completed" {
		t.Fatalf("task-002 status = %q, want completed", got)
	}
	if got := tasksByID["task-002"].PrURL; got != "https://example.test/pr/2" {
		t.Fatalf("task-002 pr_url = %q", got)
	}
	if got := tasksByID["task-002"].Body; got != "<!-- merge_commit: abc123 -->" {
		t.Fatalf("task-002 body = %q, want merge_commit comment", got)
	}

	wantReasons := []string{"worker blocked: task-001", "worker completed: task-002"}
	if !reflect.DeepEqual(trigger.reasons, wantReasons) {
		t.Fatalf("trigger reasons = %#v, want %#v", trigger.reasons, wantReasons)
	}
}

func TestQueueRunContinuesAfterProcessError(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "First", Status: "in_progress", WorkerID: "worker-1"}); err != nil {
		t.Fatalf("Add task-001: %v", err)
	}
	if err := l.Add(ledger.Task{ID: "task-002", Title: "Second", Status: "in_progress", WorkerID: "worker-2"}); err != nil {
		t.Fatalf("Add task-002: %v", err)
	}
	trigger := &fakeTrigger{}
	q := NewQueue(l, trigger, 3)

	ctx := context.Background()
	if err := q.Enqueue(ctx, Event{Type: Completed, TaskID: "task-001", WorkerID: "worker-stale"}); err != nil {
		t.Fatalf("Enqueue stale: %v", err)
	}
	if err := q.Enqueue(ctx, Event{Type: Completed, TaskID: "task-002", WorkerID: "worker-2"}); err != nil {
		t.Fatalf("Enqueue valid: %v", err)
	}
	q.Close()
	if err := q.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	tasksByID := loadTasksByID(t, l)
	if got := tasksByID["task-001"].Status; got != "in_progress" {
		t.Fatalf("task-001 status = %q, want in_progress", got)
	}
	if got := tasksByID["task-002"].Status; got != "completed" {
		t.Fatalf("task-002 status = %q, want completed", got)
	}
	if !reflect.DeepEqual(trigger.reasons, []string{"worker completed: task-002"}) {
		t.Fatalf("trigger reasons = %#v", trigger.reasons)
	}
}

func TestQueueCloseIsIdempotentAndEnqueueAfterCloseFails(t *testing.T) {
	q := NewQueue(newTestLedger(t), nil, 1)
	q.Close()
	q.Close()

	err := q.Enqueue(context.Background(), Event{Type: Completed, TaskID: "task-001", WorkerID: "worker-1"})
	if err == nil {
		t.Fatal("Enqueue after Close error = nil, want error")
	}
}

func TestQueueCloseUnblocksPendingEnqueue(t *testing.T) {
	q := NewQueue(newTestLedger(t), nil, 0)
	started := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		close(started)
		errCh <- q.Enqueue(context.Background(), Event{Type: Completed, TaskID: "task-001", WorkerID: "worker-1"})
	}()
	<-started
	time.Sleep(10 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		q.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind pending Enqueue")
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrQueueClosed) {
			t.Fatalf("Enqueue error = %v, want ErrQueueClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Enqueue did not unblock after Close")
	}
}

func TestQueueRunDoesNotProcessBufferedEventAfterCancellation(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "in_progress", WorkerID: "worker-1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q := NewQueue(l, &fakeTrigger{}, 1)
	if err := q.Enqueue(context.Background(), Event{Type: Completed, TaskID: "task-001", WorkerID: "worker-1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Run(ctx); err == nil {
		t.Fatal("Run canceled context error = nil, want error")
	}

	task := loadTasksByID(t, l)["task-001"]
	if task.Status != "in_progress" {
		t.Fatalf("task status = %q, want in_progress", task.Status)
	}
}

func TestQueueProcessSplitRequestCreatesChildrenAndSplitsParent(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Parent", Status: "in_progress", WorkerID: "worker-1", Branch: "branch-1"}); err != nil {
		t.Fatalf("Add parent: %v", err)
	}
	trigger := &fakeTrigger{}
	q := NewQueue(l, trigger, 0)

	err := q.Process(context.Background(), Event{
		Type:     SplitRequest,
		TaskID:   "task-001",
		WorkerID: "worker-1",
		Reason:   "too broad",
		ProposedSlices: []Slice{
			{
				Title:          "Child A",
				Description:    "Do A",
				AllowedFiles:   []string{"server/internal/event"},
				ForbiddenFiles: []string{"server/internal/event/forbidden.go"},
			},
			{
				Title:        "Child B",
				Description:  "Do B",
				AllowedFiles: []string{"server/internal/orchestrator"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Process split_request: %v", err)
	}

	tasks := loadTasksByID(t, l)
	parent := tasks["task-001"]
	if parent.Status != "split" || parent.Reason != "too broad" || parent.WorkerID != "" || parent.Branch != "" {
		t.Fatalf("parent after split = %#v", parent)
	}
	var childTitles []string
	for id, task := range tasks {
		if id == "task-001" {
			continue
		}
		if task.Status != "unstarted" {
			t.Fatalf("child %s status = %q, want unstarted", id, task.Status)
		}
		childTitles = append(childTitles, task.Title)
	}
	sort.Strings(childTitles)
	if !reflect.DeepEqual(childTitles, []string{"Child A", "Child B"}) {
		t.Fatalf("child titles = %#v", childTitles)
	}
	if !reflect.DeepEqual(trigger.reasons, []string{"worker split_request: task-001"}) {
		t.Fatalf("trigger reasons = %#v", trigger.reasons)
	}
}

func TestQueueProcessRejectsStaleWorkerEvent(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "in_progress", WorkerID: "worker-current"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q := NewQueue(l, &fakeTrigger{}, 0)

	err := q.Process(context.Background(), Event{Type: Completed, TaskID: "task-001", WorkerID: "worker-stale"})
	if err == nil {
		t.Fatal("Process stale worker completed error = nil, want error")
	}
	task := loadTasksByID(t, l)["task-001"]
	if task.Status != "in_progress" || task.WorkerID != "worker-current" {
		t.Fatalf("task changed after stale worker event: %#v", task)
	}
}

func TestQueueProcessRequiresWorkerID(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "in_progress", WorkerID: "worker-current"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q := NewQueue(l, &fakeTrigger{}, 0)

	err := q.Process(context.Background(), Event{Type: Completed, TaskID: "task-001"})
	if err == nil {
		t.Fatal("Process missing worker_id error = nil, want error")
	}
	task := loadTasksByID(t, l)["task-001"]
	if task.Status != "in_progress" || task.WorkerID != "worker-current" {
		t.Fatalf("task changed after missing worker_id event: %#v", task)
	}
}

func TestQueueProcessSplitRequestRejectsStaleWorkerAndRollsBackChildren(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Parent", Status: "in_progress", WorkerID: "worker-current"}); err != nil {
		t.Fatalf("Add parent: %v", err)
	}
	q := NewQueue(l, &fakeTrigger{}, 0)

	err := q.Process(context.Background(), Event{
		Type:     SplitRequest,
		TaskID:   "task-001",
		WorkerID: "worker-stale",
		ProposedSlices: []Slice{
			{Title: "Child", AllowedFiles: []string{"server/internal/event"}},
		},
	})
	if err == nil {
		t.Fatal("Process split_request stale worker error = nil, want error")
	}
	tasks := loadTasksByID(t, l)
	if len(tasks) != 1 {
		t.Fatalf("stale split_request left child tasks: %#v", tasks)
	}
	parent := tasks["task-001"]
	if parent.Status != "in_progress" || parent.WorkerID != "worker-current" {
		t.Fatalf("parent changed after stale split_request: %#v", parent)
	}
}

func TestQueueProcessSplitRequestRollsBackChildrenWhenParentUpdateFails(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Parent", Status: "completed", WorkerID: "worker-1"}); err != nil {
		t.Fatalf("Add parent: %v", err)
	}
	q := NewQueue(l, &fakeTrigger{}, 0)

	err := q.Process(context.Background(), Event{
		Type:     SplitRequest,
		TaskID:   "task-001",
		WorkerID: "worker-1",
		ProposedSlices: []Slice{
			{Title: "Child", AllowedFiles: []string{"server/internal/event"}},
		},
	})
	if err == nil {
		t.Fatal("Process split_request on completed parent error = nil, want error")
	}
	tasks := loadTasksByID(t, l)
	if len(tasks) != 1 {
		t.Fatalf("split rollback left child tasks: %#v", tasks)
	}
	if tasks["task-001"].Status != "completed" {
		t.Fatalf("parent status = %q, want completed", tasks["task-001"].Status)
	}
}

func newTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	dir := t.TempDir()
	return ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
}

func loadTasksByID(t *testing.T, l *ledger.Ledger) map[string]ledger.Task {
	t.Helper()
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := make(map[string]ledger.Task, len(tasks))
	for _, task := range tasks {
		out[task.ID] = task
	}
	return out
}
