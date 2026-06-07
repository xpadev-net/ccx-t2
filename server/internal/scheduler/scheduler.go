package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/xpadev/ccx-t2/internal/ledger"
)

const heartbeatReason = "heartbeat"

// Triggerer starts or wakes the orchestrator.
type Triggerer interface {
	Trigger(ctx context.Context, reason string) error
}

// Scheduler periodically wakes the orchestrator while actionable tasks exist.
type Scheduler struct {
	ledger   *ledger.Ledger
	trigger  Triggerer
	interval time.Duration
}

// New creates a scheduler that checks the ledger on the given interval.
func New(l *ledger.Ledger, trigger Triggerer, interval time.Duration) *Scheduler {
	return &Scheduler{
		ledger:   l,
		trigger:  trigger,
		interval: interval,
	}
}

// Run triggers heartbeat orchestration until no unstarted, in_progress, or
// blocked tasks remain. Split tasks are intentionally ignored.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		active, err := s.hasActionableTasks()
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		if err := s.trigger.Trigger(ctx, heartbeatReason); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			log.Printf("warn: scheduler heartbeat trigger failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) validate() error {
	if s.ledger == nil {
		return fmt.Errorf("scheduler ledger is nil")
	}
	if s.trigger == nil {
		return fmt.Errorf("scheduler trigger is nil")
	}
	if s.interval <= 0 {
		return fmt.Errorf("scheduler interval must be positive")
	}
	return nil
}

func (s *Scheduler) hasActionableTasks() (bool, error) {
	tasks, err := s.ledger.Load()
	if err != nil {
		return false, fmt.Errorf("load ledger: %w", err)
	}
	for _, task := range tasks {
		switch task.Status {
		case "unstarted", "in_progress", "blocked":
			return true, nil
		}
	}
	return false, nil
}
