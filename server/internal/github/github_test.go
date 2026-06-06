package github

import (
	"testing"

	gh "github.com/google/go-github/v60/github"
)

func TestNormalizeCheckStatusTreatsNonTerminalStatusesAsPending(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "waiting", "requested", "pending"} {
		t.Run(status, func(t *testing.T) {
			run := &gh.CheckRun{Status: &status}
			if got := normalizeCheckStatus(run); got != CheckPending {
				t.Fatalf("normalizeCheckStatus(%q) = %s, want %s", status, got, CheckPending)
			}
		})
	}
}

func TestNormalizeCheckStatusTreatsUnknownEmptyConclusionAsFailure(t *testing.T) {
	status := "unknown"
	run := &gh.CheckRun{Status: &status}
	if got := normalizeCheckStatus(run); got != CheckFailure {
		t.Fatalf("normalizeCheckStatus(%q) = %s, want %s", status, got, CheckFailure)
	}
}
