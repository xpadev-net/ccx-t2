package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xpadev/ccx-t2/internal/ledger"
)

type fakeTrigger struct {
	reasons []string
	errs    []error
	onCall  func(int)
}

func (f *fakeTrigger) Trigger(ctx context.Context, reason string) error {
	f.reasons = append(f.reasons, reason)
	call := len(f.reasons)
	if f.onCall != nil {
		f.onCall(call)
	}
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func TestRunReturnsWithoutTriggerWhenNoActionableTasks(t *testing.T) {
	l := newTestLedger(t)
	for _, task := range []ledger.Task{
		{ID: "task-001", Title: "Done", Status: "completed"},
		{ID: "task-002", Title: "Split", Status: "split"},
	} {
		if err := l.Add(task); err != nil {
			t.Fatalf("Add %s: %v", task.ID, err)
		}
	}
	trigger := &fakeTrigger{}
	s := New(l, trigger, time.Millisecond)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trigger.reasons) != 0 {
		t.Fatalf("trigger reasons = %#v, want none", trigger.reasons)
	}
}

func TestRunTriggersHeartbeatUntilNoActionableTasksRemain(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	trigger := &fakeTrigger{
		onCall: func(call int) {
			if call == 2 {
				if err := l.Update("task-001", map[string]any{"status": "completed"}); err != nil {
					t.Errorf("Update: %v", err)
				}
			}
		},
	}
	s := New(l, trigger, time.Millisecond)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{heartbeatReason, heartbeatReason}; !reflect.DeepEqual(trigger.reasons, want) {
		t.Fatalf("trigger reasons = %#v, want %#v", trigger.reasons, want)
	}
}

func TestRunContinuesAfterTriggerError(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "blocked"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	triggerErr := errors.New("trigger failed")
	trigger := &fakeTrigger{
		errs: []error{triggerErr},
		onCall: func(call int) {
			if call == 2 {
				if err := l.Update("task-001", map[string]any{"status": "completed"}); err != nil {
					t.Errorf("Update: %v", err)
				}
			}
		},
	}
	s := New(l, trigger, time.Millisecond)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(trigger.reasons); got != 2 {
		t.Fatalf("trigger calls = %d, want 2", got)
	}
}

func TestRunReturnsContextErrorWhileWaiting(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "in_progress"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	trigger := &fakeTrigger{
		onCall: func(call int) {
			cancel()
		},
	}
	s := New(l, trigger, time.Hour)

	err := s.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if got := len(trigger.reasons); got != 1 {
		t.Fatalf("trigger calls = %d, want 1", got)
	}
}

func TestRunValidatesDependencies(t *testing.T) {
	err := New(nil, &fakeTrigger{}, time.Millisecond).Run(context.Background())
	if err == nil {
		t.Fatal("Run nil ledger error = nil, want error")
	}
	l := newTestLedger(t)
	err = New(l, nil, time.Millisecond).Run(context.Background())
	if err == nil {
		t.Fatal("Run nil trigger error = nil, want error")
	}
	err = New(l, &fakeTrigger{}, 0).Run(context.Background())
	if err == nil {
		t.Fatal("Run non-positive interval error = nil, want error")
	}
}

func newTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	dir := t.TempDir()
	return ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
}
