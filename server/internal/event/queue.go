package event

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/xpadev/ccx-t2/internal/ledger"
)

var ErrQueueClosed = errors.New("event queue is closed")

// Type identifies the worker event kind.
type Type string

const (
	Completed    Type = "completed"
	Blocked      Type = "blocked"
	SplitRequest Type = "split_request"
)

// Slice describes one child task requested by a split_request event.
type Slice struct {
	Title          string
	Description    string
	AllowedFiles   []string
	ForbiddenFiles []string
}

// Event is a worker notification queued for serial processing.
type Event struct {
	Type           Type
	TaskID         string
	WorkerID       string
	PRURL          string
	MergeCommit    string
	Reason         string
	ProposedSlices []Slice
}

// Triggerer starts or wakes the orchestrator after an event changes the ledger.
type Triggerer interface {
	Trigger(ctx context.Context, reason string) error
}

// Queue serializes worker events and triggers the orchestrator after each
// successful ledger update.
type Queue struct {
	ledger  *ledger.Ledger
	trigger Triggerer
	buffer  int
	events  []Event
	mu      sync.Mutex
	cond    *sync.Cond
	closed  bool
	waiting int
}

// NewQueue creates a FIFO queue with the given buffer size.
func NewQueue(l *ledger.Ledger, trigger Triggerer, buffer int) *Queue {
	if buffer < 0 {
		buffer = 0
	}
	q := &Queue{
		ledger:  l,
		trigger: trigger,
		buffer:  buffer,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Enqueue appends an event to the FIFO queue or returns ctx.Err if canceled.
func (q *Queue) Enqueue(ctx context.Context, e Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	cancelWake := q.wakeOnCancel(ctx)
	defer cancelWake()

	for {
		if q.closed {
			return ErrQueueClosed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if q.canAcceptLocked() {
			q.events = append(q.events, e)
			q.cond.Signal()
			return nil
		}
		q.cond.Wait()
	}
}

func (q *Queue) canAcceptLocked() bool {
	if q.buffer == 0 {
		return q.waiting > 0 && len(q.events) == 0
	}
	return len(q.events) < q.buffer
}

// Close closes the queue. Run exits after already queued events are processed.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

// Run processes queued events one at a time until the queue is closed or ctx is
// canceled. Processing errors are logged and do not stop later events.
func (q *Queue) Run(ctx context.Context) error {
	for {
		e, ok, err := q.next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		q.processAndLog(ctx, e)
	}
}

func (q *Queue) next(ctx context.Context) (Event, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	cancelWake := q.wakeOnCancel(ctx)
	defer cancelWake()

	for len(q.events) == 0 && !q.closed {
		if err := ctx.Err(); err != nil {
			return Event{}, false, err
		}
		q.waiting++
		q.cond.Broadcast()
		q.cond.Wait()
		q.waiting--
	}
	if err := ctx.Err(); err != nil {
		return Event{}, false, err
	}
	if len(q.events) == 0 && q.closed {
		return Event{}, false, nil
	}
	e := q.events[0]
	copy(q.events, q.events[1:])
	q.events = q.events[:len(q.events)-1]
	q.cond.Signal()
	return e, true, nil
}

func (q *Queue) wakeOnCancel(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.cond.Broadcast()
			q.mu.Unlock()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func (q *Queue) processAndLog(ctx context.Context, e Event) {
	if err := q.Process(ctx, e); err != nil {
		log.Printf("warn: event queue failed to process event type=%s task_id=%s worker_id=%s: %v",
			e.Type, e.TaskID, e.WorkerID, err)
	}
}

// Process applies one event to the ledger and triggers the orchestrator.
func (q *Queue) Process(ctx context.Context, e Event) error {
	if q.ledger == nil {
		return fmt.Errorf("event queue ledger is nil")
	}
	if e.TaskID == "" {
		return fmt.Errorf("event task_id is required")
	}
	if e.WorkerID == "" {
		return fmt.Errorf("event worker_id is required")
	}

	var err error
	switch e.Type {
	case Completed:
		_, err = q.ledger.UpdateIfStatusesReturnPrevWith(e.TaskID, []string{"in_progress", "blocked"},
			func(current ledger.Task) (map[string]any, error) {
				if err := verifyWorkerOwner(current, e); err != nil {
					return nil, err
				}
				fields := map[string]any{
					"status":    "completed",
					"pr_url":    e.PRURL,
					"worker_id": "",
					"harness":   "",
					"reason":    "",
				}
				if e.MergeCommit != "" {
					comment := "<!-- merge_commit: " + e.MergeCommit + " -->"
					if current.Body == "" {
						fields["body"] = comment
					} else {
						fields["body"] = current.Body + "\n" + comment
					}
				}
				return fields, nil
			},
		)
	case Blocked:
		_, err = q.ledger.UpdateIfStatusesReturnPrevWith(e.TaskID, []string{"in_progress", "blocked"},
			func(current ledger.Task) (map[string]any, error) {
				if err := verifyWorkerOwner(current, e); err != nil {
					return nil, err
				}
				fields := map[string]any{"status": "blocked"}
				if e.Reason != "" {
					fields["reason"] = e.Reason
				}
				return fields, nil
			},
		)
	case SplitRequest:
		err = q.processSplitRequest(ctx, e)
	default:
		return fmt.Errorf("unknown event type: %s", e.Type)
	}
	if err != nil {
		return err
	}
	if q.trigger != nil {
		return q.trigger.Trigger(ctx, triggerReason(e))
	}
	return nil
}

func (q *Queue) processSplitRequest(ctx context.Context, e Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(e.ProposedSlices) == 0 {
		return fmt.Errorf("proposed_slices must not be empty for split_request")
	}
	children := make([]ledger.Task, len(e.ProposedSlices))
	for i, s := range e.ProposedSlices {
		if s.Title == "" {
			return fmt.Errorf("proposed_slices[%d]: title is required", i)
		}
		if err := ledger.ValidatePaths(s.AllowedFiles); err != nil {
			return fmt.Errorf("proposed_slices[%d] allowed_files: %w", i, err)
		}
		if err := ledger.ValidatePaths(s.ForbiddenFiles); err != nil {
			return fmt.Errorf("proposed_slices[%d] forbidden_files: %w", i, err)
		}
		children[i] = ledger.Task{
			Title:          s.Title,
			Status:         "unstarted",
			AllowedFiles:   s.AllowedFiles,
			ForbiddenFiles: s.ForbiddenFiles,
			Body:           s.Description,
		}
	}
	ids, err := q.ledger.AddAllNew(children)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		if rollbackErr := q.ledger.DeleteTasks(ids); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rollback split children: %w", rollbackErr))
		}
		return err
	}
	if _, err := q.ledger.UpdateIfStatusesReturnPrevWith(e.TaskID, []string{"in_progress", "blocked"},
		func(current ledger.Task) (map[string]any, error) {
			if err := verifyWorkerOwner(current, e); err != nil {
				return nil, err
			}
			return map[string]any{
				"status":    "split",
				"reason":    e.Reason,
				"worker_id": "",
				"branch":    "",
				"harness":   "",
			}, nil
		},
	); err != nil {
		if rollbackErr := q.ledger.DeleteTasks(ids); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rollback split children: %w", rollbackErr))
		}
		return err
	}
	return nil
}

func verifyWorkerOwner(task ledger.Task, e Event) error {
	if task.WorkerID == e.WorkerID {
		return nil
	}
	return fmt.Errorf("worker %q is not assigned to task %s (assigned to %q)",
		e.WorkerID, e.TaskID, task.WorkerID)
}

func triggerReason(e Event) string {
	switch e.Type {
	case Completed:
		return "worker completed: " + e.TaskID
	case Blocked:
		return "worker blocked: " + e.TaskID
	case SplitRequest:
		return "worker split_request: " + e.TaskID
	default:
		return "worker event: " + e.TaskID
	}
}
