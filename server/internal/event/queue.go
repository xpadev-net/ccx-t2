package event

import (
	"context"
	"errors"
	"fmt"

	"github.com/xpadev/ccx-t2/internal/ledger"
)

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
	events  chan Event
}

// NewQueue creates a FIFO queue with the given buffer size.
func NewQueue(l *ledger.Ledger, trigger Triggerer, buffer int) *Queue {
	if buffer < 0 {
		buffer = 0
	}
	return &Queue{
		ledger:  l,
		trigger: trigger,
		events:  make(chan Event, buffer),
	}
}

// Enqueue appends an event to the FIFO queue or returns ctx.Err if canceled.
func (q *Queue) Enqueue(ctx context.Context, e Event) error {
	select {
	case q.events <- e:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close closes the queue. Run exits after already queued events are processed.
func (q *Queue) Close() {
	close(q.events)
}

// Run processes queued events one at a time until the queue is closed or ctx is
// canceled. The first processing error stops the loop and is returned.
func (q *Queue) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-q.events:
			if !ok {
				return nil
			}
			if err := q.Process(ctx, e); err != nil {
				return err
			}
		}
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
		err = q.processSplitRequest(e)
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

func (q *Queue) processSplitRequest(e Event) error {
	if len(e.ProposedSlices) == 0 {
		return fmt.Errorf("proposed_slices must not be empty for split_request")
	}
	children := make([]ledger.Task, len(e.ProposedSlices))
	ids, err := q.ledger.GenerateIDs(len(e.ProposedSlices))
	if err != nil {
		return err
	}
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
			ID:             ids[i],
			Title:          s.Title,
			Status:         "unstarted",
			AllowedFiles:   s.AllowedFiles,
			ForbiddenFiles: s.ForbiddenFiles,
			Body:           s.Description,
		}
	}
	if err := q.ledger.AddAll(children); err != nil {
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
