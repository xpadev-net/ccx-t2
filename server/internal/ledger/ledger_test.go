package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- Parser tests ----

func TestParseBodyHorizontalRule(t *testing.T) {
	content := `---
id: task-001
title: Test
status: unstarted
---

Body text.

---

More body.

`
	tasks, err := parseContent(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if !strings.Contains(tasks[0].Body, "---") {
		t.Errorf("expected body to contain horizontal rule, got: %q", tasks[0].Body)
	}
}

func TestParseBodyHrNotFrontMatter(t *testing.T) {
	// "---" followed by a non-"id:" line must stay as body.
	content := `---
id: task-001
title: Test
status: unstarted
---

Normal line.

---
not-id: value

`
	tasks, err := parseContent(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if !strings.Contains(tasks[0].Body, "not-id: value") {
		t.Errorf("body should contain the non-id line, got: %q", tasks[0].Body)
	}
}

func TestParseMultipleTasks(t *testing.T) {
	content := `---
id: task-001
title: First
status: unstarted
---

First body.

---
id: task-002
title: Second
status: in_progress
---

Second body.
`
	tasks, err := parseContent(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	if tasks[0].ID != "task-001" {
		t.Errorf("expected task-001, got %s", tasks[0].ID)
	}
	if tasks[1].ID != "task-002" {
		t.Errorf("expected task-002, got %s", tasks[1].ID)
	}
}

func TestParseEOFWithoutClosingSeparator(t *testing.T) {
	// Last task's body ends at EOF without closing "---".
	content := `---
id: task-001
title: No closing sep
status: unstarted
---

Body without closing separator`
	tasks, err := parseContent(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Body != "Body without closing separator" {
		t.Errorf("unexpected body: %q", tasks[0].Body)
	}
}

func TestPathMatchesSupportsDoubleStarDirectorySuffix(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		file    string
		want    bool
	}{
		{
			name:    "matches child file",
			pattern: "server/internal/mcp/**",
			file:    "server/internal/mcp/handlers.go",
			want:    true,
		},
		{
			name:    "matches nested child file",
			pattern: "server/internal/mcp/**",
			file:    "server/internal/mcp/testdata/input.json",
			want:    true,
		},
		{
			name:    "keeps directory boundary",
			pattern: "server/internal/mcp/**",
			file:    "server/internal/mcpfoo/handlers.go",
			want:    false,
		},
		{
			name:    "normalizes backslashes",
			pattern: `server\internal\mcp\**`,
			file:    "server/internal/mcp/handlers.go",
			want:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathMatches(tc.pattern, tc.file); got != tc.want {
				t.Fatalf("PathMatches(%q, %q) = %v, want %v", tc.pattern, tc.file, got, tc.want)
			}
		})
	}
}

func TestParseUnclosedFrontMatterReturnsError(t *testing.T) {
	content := `---
id: task-001
title: Truncated
status: unstarted
`
	_, err := parseContent(content)
	if err == nil {
		t.Error("expected error for unclosed front matter, got nil")
	}
}

// ---- Round-trip test ----

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ledgerFile := filepath.Join(dir, "ledger.md")
	archiveDir := filepath.Join(dir, "archive")

	l := NewLedger(ledgerFile, archiveDir)

	tasks := []Task{
		{ID: "task-001", Title: "First task", Status: "unstarted", Body: "First body."},
		{ID: "task-002", Title: "Second task", Status: "in_progress", Body: "Second body.\n\n---\n\nWith HR."},
	}

	if err := l.Save(tasks); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 tasks after round-trip, got %d", len(loaded))
	}
	if loaded[0].ID != "task-001" || loaded[1].ID != "task-002" {
		t.Errorf("task IDs mismatch after round-trip")
	}
	if !strings.Contains(loaded[1].Body, "---") {
		t.Errorf("body HR not preserved: %q", loaded[1].Body)
	}
}

// ---- updated_at tests ----

func TestAddUpdatesUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	before := time.Now().Add(-time.Second)
	err := l.Add(Task{ID: "task-001", Title: "T", Status: "unstarted"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	after := time.Now().Add(time.Second)

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ts, err := time.Parse(time.RFC3339, tasks[0].UpdatedAt)
	if err != nil {
		t.Fatalf("parse updated_at: %v", err)
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("updated_at %v not in expected range [%v, %v]", ts, before, after)
	}
}

func TestUpdateUpdatesUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	firstUpdatedAt := tasks[0].UpdatedAt

	time.Sleep(1100 * time.Millisecond)
	if err := l.Update("task-001", map[string]any{"title": "Updated"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	tasks, err = l.Load()
	if err != nil {
		t.Fatalf("Load after Update: %v", err)
	}
	if tasks[0].UpdatedAt == firstUpdatedAt {
		t.Error("updated_at not changed after Update")
	}
}

func TestAddDuplicateReturnsErrTaskExists(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := l.Add(Task{ID: "task-001", Title: "Duplicate", Status: "unstarted"})
	if !errors.Is(err, ErrTaskExists) {
		t.Fatalf("Add duplicate error = %v, want ErrTaskExists", err)
	}
}

func TestUpdateMissingReturnsErrTaskNotFound(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	_, err := l.UpdateReturnPrev("missing", map[string]any{"title": "Updated"})
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("UpdateReturnPrev missing error = %v, want ErrTaskNotFound", err)
	}
}

func TestDeleteTaskReturnPrevRemovesTask(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "unstarted", Body: "body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	prev, err := l.DeleteTaskReturnPrev("task-001")
	if err != nil {
		t.Fatalf("DeleteTaskReturnPrev: %v", err)
	}
	if prev.ID != "task-001" || prev.Body != "body" {
		t.Fatalf("deleted task = %#v, want task-001 with body", prev)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0", len(tasks))
	}
}

func TestDeleteTaskReturnPrevMissingReturnsErrTaskNotFound(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	_, err := l.DeleteTaskReturnPrev("missing")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("DeleteTaskReturnPrev missing error = %v, want ErrTaskNotFound", err)
	}
}

func TestPrepareDeleteReturnPrevDeletesUnmarkedTask(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	prev, marker, marked, err := l.PrepareDeleteReturnPrev("task-001", func(Task) bool { return false })
	if err != nil {
		t.Fatalf("PrepareDeleteReturnPrev: %v", err)
	}
	if marker.ID != "" {
		t.Fatalf("marker = %#v, want empty marker", marker)
	}
	if marked {
		t.Fatal("marked = true, want false")
	}
	if prev.ID != "task-001" {
		t.Fatalf("prev.ID = %q, want task-001", prev.ID)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0", len(tasks))
	}
}

func TestPrepareDeleteReturnPrevMarksTask(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "in_progress", WorkerID: "worker-task-001"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	prev, marker, marked, err := l.PrepareDeleteReturnPrev("task-001", func(Task) bool { return true })
	if err != nil {
		t.Fatalf("PrepareDeleteReturnPrev: %v", err)
	}
	if !marked {
		t.Fatal("marked = false, want true")
	}
	if prev.Status != "in_progress" || prev.WorkerID != "worker-task-001" {
		t.Fatalf("prev = %#v, want original worker snapshot", prev)
	}
	if marker.Status != "deleting" || marker.UpdatedAt == "" {
		t.Fatalf("marker = %#v, want deleting marker with updated_at", marker)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Status != "deleting" || tasks[0].WorkerID != "worker-task-001" {
		t.Fatalf("tasks = %#v, want deleting marker with worker metadata", tasks)
	}
}

func TestPrepareDeleteReturnPrevKeepsDeletingTask(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "deleting", WorkerID: "worker-task-001"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	prev, marker, marked, err := l.PrepareDeleteReturnPrev("task-001", func(Task) bool { return false })
	if !errors.Is(err, ErrTaskDeleteInProgress) {
		t.Fatalf("PrepareDeleteReturnPrev error = %v, want ErrTaskDeleteInProgress", err)
	}
	if marked {
		t.Fatal("marked = true, want false")
	}
	if prev.ID != "task-001" || prev.Status != "deleting" {
		t.Fatalf("prev = %#v, want deleting task snapshot", prev)
	}
	if marker.ID != "task-001" || marker.Status != "deleting" {
		t.Fatalf("marker = %#v, want deleting task snapshot", marker)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task-001" || tasks[0].Status != "deleting" {
		t.Fatalf("tasks = %#v, want deleting task retained", tasks)
	}
}

func TestDeleteTaskIfCurrentRequiresMatchingMarker(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "in_progress"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, marker, marked, err := l.PrepareDeleteReturnPrev("task-001", func(Task) bool { return true })
	if err != nil {
		t.Fatalf("PrepareDeleteReturnPrev: %v", err)
	}
	if !marked {
		t.Fatal("marked = false, want true")
	}
	stale := marker
	stale.UpdatedAt = "2026-06-07T00:00:00Z"
	deleted, err := l.DeleteTaskIfCurrent(stale)
	if err != nil {
		t.Fatalf("DeleteTaskIfCurrent stale: %v", err)
	}
	if deleted {
		t.Fatal("deleted stale marker = true, want false")
	}
	deleted, err = l.DeleteTaskIfCurrent(marker)
	if err != nil {
		t.Fatalf("DeleteTaskIfCurrent: %v", err)
	}
	if !deleted {
		t.Fatal("deleted = false, want true")
	}
}

func TestRestoreTaskSnapshotIfCurrentRequiresMatchingMarker(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	original := Task{ID: "task-001", Title: "T", Status: "in_progress"}

	if err := l.Add(original); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, marker, marked, err := l.PrepareDeleteReturnPrev("task-001", func(Task) bool { return true })
	if err != nil {
		t.Fatalf("PrepareDeleteReturnPrev: %v", err)
	}
	if !marked {
		t.Fatal("marked = false, want true")
	}
	stale := marker
	stale.UpdatedAt = "2026-06-07T00:00:00Z"
	restored, err := l.RestoreTaskSnapshotIfCurrent(original, stale)
	if err != nil {
		t.Fatalf("RestoreTaskSnapshotIfCurrent stale: %v", err)
	}
	if restored {
		t.Fatal("restored stale marker = true, want false")
	}
	restored, err = l.RestoreTaskSnapshotIfCurrent(original, marker)
	if err != nil {
		t.Fatalf("RestoreTaskSnapshotIfCurrent: %v", err)
	}
	if !restored {
		t.Fatal("restored = false, want true")
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Status != "in_progress" {
		t.Fatalf("tasks = %#v, want original task restored", tasks)
	}
}

func TestRestoreTaskSnapshotRestoresSnapshot(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	task := Task{ID: "task-001", Title: "T", Status: "in_progress", UpdatedAt: "2026-06-07T00:00:00Z", Body: "body"}

	if err := l.RestoreTaskSnapshot(task); err != nil {
		t.Fatalf("RestoreTaskSnapshot: %v", err)
	}
	if err := l.RestoreTaskSnapshot(Task{ID: "task-001", Title: "Restored", Status: "blocked"}); err != nil {
		t.Fatalf("second RestoreTaskSnapshot: %v", err)
	}
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].Title != "Restored" || tasks[0].Status != "blocked" {
		t.Fatalf("restored task = %#v, want replacement snapshot", tasks[0])
	}
}

// ---- Archive tests ----

func TestArchiveSlug(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"Add auth endpoint", "add-auth-endpoint"},
		{"Fix: #1 bug", "fix-1-bug"},
		{"Hello World!", "hello-world"},
		{"", "untitled"},
		{"---", "untitled"},
		{"my-task name", "my-task-name"},
	}
	for _, c := range cases {
		got := titleToSlug(c.title)
		if got != c.want {
			t.Errorf("titleToSlug(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestArchiveCompletedOnly(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := l.Archive("task-001", "")
	if err == nil {
		t.Error("expected error archiving non-completed task")
	}
}

func TestArchiveMovesFile(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)

	if err := l.Add(Task{ID: "task-001", Title: "Do thing", Status: "completed", Body: "body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := l.Archive("task-001", "abc123"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Task should be removed from ledger.
	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, t2 := range tasks {
		if t2.ID == "task-001" {
			t.Error("task still in ledger after archive")
		}
	}

	// Archive file should exist.
	expected := filepath.Join(archiveDir, "task-001-do-thing.md")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("archive file not found: %v", err)
	}
}

func TestArchiveStripsMergeCommitComment(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	body := "Some body.\n<!-- merge_commit: deadbeef -->"
	if err := l.Add(Task{ID: "task-001", Title: "T", Status: "completed", Body: body}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := l.Archive("task-001", "deadbeef"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	archiveFile := filepath.Join(dir, "archive", "task-001-t.md")
	data, err := os.ReadFile(archiveFile)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if strings.Contains(string(data), "<!-- merge_commit:") {
		t.Error("merge_commit comment not stripped from archive")
	}
	if !strings.Contains(string(data), "merge_commit: deadbeef") {
		t.Error("merge_commit not in archive front matter")
	}
}

func TestArchiveIdempotentRecovery(t *testing.T) {
	// Simulate crash between archive write and ledger save: archive file exists
	// but task is still in ledger. Re-calling Archive should succeed without
	// re-writing the archive and should remove the task from the ledger.
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)

	if err := l.Add(Task{ID: "task-001", Title: "Do thing", Status: "completed", Body: "body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := l.Archive("task-001", "abc123"); err != nil {
		t.Fatalf("first Archive: %v", err)
	}

	// Simulate crash recovery: re-add the task (as if ledger save had failed).
	if err := l.Add(Task{ID: "task-001", Title: "Do thing", Status: "completed", Body: "body"}); err != nil {
		t.Fatalf("re-Add: %v", err)
	}

	// Second archive call should succeed and remove from ledger.
	if err := l.Archive("task-001", "abc123"); err != nil {
		t.Fatalf("second Archive (idempotent): %v", err)
	}

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, t2 := range tasks {
		if t2.ID == "task-001" {
			t.Error("task still in ledger after idempotent archive")
		}
	}
}

func TestArchiveAlreadyArchivedTaskIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)

	if err := l.Add(Task{ID: "task-001", Title: "Do thing", Status: "completed", Body: "body"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := l.Archive("task-001", "abc123"); err != nil {
		t.Fatalf("first Archive: %v", err)
	}
	if err := l.Archive("task-001", "abc123"); err != nil {
		t.Fatalf("second Archive for already archived task: %v", err)
	}
}

func TestArchivedTaskReturnsArchivedMetadata(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)

	if err := l.Add(Task{ID: "task-001", Title: "Do thing", Status: "completed", Branch: "feature/done"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := l.Archive("task-001", "abc123"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	task, ok, err := l.ArchivedTask("task-001")
	if err != nil {
		t.Fatalf("ArchivedTask: %v", err)
	}
	if !ok {
		t.Fatal("ArchivedTask ok = false, want true")
	}
	if task.Branch != "feature/done" {
		t.Fatalf("ArchivedTask branch = %q, want feature/done", task.Branch)
	}
}

func TestArchiveAlreadyArchivedRequiresExactID(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archiveDir, "task-20260607-0001-title.md"), []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)
	if err := l.Archive("task-20260607", ""); err == nil {
		t.Fatal("Archive partial archived ID error = nil, want task not found")
	}
	archived, err := l.IsArchived("task-20260607")
	if err != nil {
		t.Fatalf("IsArchived(partial ID): %v", err)
	}
	if archived {
		t.Fatal("IsArchived(partial ID) = true, want false")
	}
	archived, err = l.IsArchived("task-20260607-0001")
	if err != nil {
		t.Fatalf("IsArchived(exact ID): %v", err)
	}
	if !archived {
		t.Fatal("IsArchived(exact ID) = false, want true")
	}
}

// ---- GenerateID tests ----

func TestGenerateIDUnique(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		id, err := l.GenerateID()
		if err != nil {
			t.Fatalf("GenerateID: %v", err)
		}
		if seen[id] {
			t.Errorf("duplicate ID: %s", id)
		}
		seen[id] = true
		if err := l.Add(Task{ID: id, Title: "T", Status: "unstarted"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
}

func TestGenerateIDNoConflictWithArchive(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	date := time.Now().Format("20060102")
	existingID := "task-" + date + "-0001"
	// Create a fake archive file with this ID.
	if err := os.WriteFile(filepath.Join(archiveDir, existingID+"-some-slug.md"), []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)
	id, err := l.GenerateID()
	if err != nil {
		t.Fatalf("GenerateID: %v", err)
	}
	if id == existingID {
		t.Errorf("GenerateID returned conflicting ID %s", id)
	}
}

func TestAddNewNoConflictWithArchive(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	date := time.Now().Format("20060102")
	existingID := "task-" + date + "-0001"
	if err := os.WriteFile(filepath.Join(archiveDir, existingID+"-archived.md"), []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)
	id, err := l.AddNew(Task{Title: "New", Status: "unstarted"})
	if err != nil {
		t.Fatalf("AddNew: %v", err)
	}
	if id == existingID {
		t.Errorf("AddNew returned archived ID %s", id)
	}
}

func TestAddAllNewGeneratesIDsAndAppendsAtomically(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)

	ids, err := l.AddAllNew([]Task{
		{Title: "First", Status: "unstarted"},
		{Title: "Second", Status: "unstarted"},
	})
	if err != nil {
		t.Fatalf("AddAllNew: %v", err)
	}
	if len(ids) != 2 || ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("ids = %#v, want two distinct ids", ids)
	}

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 2 || tasks[0].ID != ids[0] || tasks[1].ID != ids[1] {
		t.Fatalf("tasks = %#v, ids = %#v", tasks, ids)
	}
}

func TestUpdateIfStatusesReturnPrevReturnsSnapshotAndRejectsDisallowedStatus(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{
		ID:       "task-001",
		Title:    "Before",
		Status:   "in_progress",
		WorkerID: "worker-old",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	prev, err := l.UpdateIfStatusesReturnPrev("task-001", []string{"in_progress"}, map[string]any{
		"status":    "completed",
		"title":     "After",
		"worker_id": "",
	})
	if err != nil {
		t.Fatalf("UpdateIfStatusesReturnPrev: %v", err)
	}
	if prev.Status != "in_progress" || prev.WorkerID != "worker-old" || prev.Title != "Before" {
		t.Fatalf("prev snapshot mismatch: %#v", prev)
	}

	if _, err := l.UpdateIfStatusesReturnPrev("task-001", []string{"in_progress"}, map[string]any{
		"status": "blocked",
	}); err == nil {
		t.Fatal("UpdateIfStatusesReturnPrev with disallowed status error = nil, want error")
	}

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tasks[0].Status != "completed" || tasks[0].Title != "After" {
		t.Fatalf("task changed after rejected update or first update failed: %#v", tasks[0])
	}
}

func TestUpdateIfStatusesReturnPrevWithComputesFieldsFromCurrentSnapshot(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{
		ID:     "task-001",
		Title:  "Task",
		Status: "in_progress",
		Body:   "current body",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	prev, err := l.UpdateIfStatusesReturnPrevWith("task-001", []string{"in_progress"}, func(current Task) (map[string]any, error) {
		if current.Body != "current body" {
			t.Fatalf("updater saw body %q, want current body", current.Body)
		}
		return map[string]any{
			"status": "completed",
			"body":   current.Body + "\n<!-- merge_commit: abc123 -->",
		}, nil
	})
	if err != nil {
		t.Fatalf("UpdateIfStatusesReturnPrevWith: %v", err)
	}
	if prev.Body != "current body" || prev.Status != "in_progress" {
		t.Fatalf("prev snapshot mismatch: %#v", prev)
	}

	called := false
	if _, err := l.UpdateIfStatusesReturnPrevWith("task-001", []string{"in_progress"}, func(current Task) (map[string]any, error) {
		called = true
		return map[string]any{"status": "blocked"}, nil
	}); err == nil {
		t.Fatal("UpdateIfStatusesReturnPrevWith disallowed status error = nil, want error")
	}
	if called {
		t.Fatal("updater was called for disallowed status")
	}

	tasks, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tasks[0].Body != "current body\n<!-- merge_commit: abc123 -->" {
		t.Fatalf("body mismatch: %q", tasks[0].Body)
	}
}

// ---- FrontMatter field order test ----

func TestIDFirstInFrontMatter(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	if err := l.Add(Task{ID: "task-001", Title: "Test", Status: "unstarted"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "ledger.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	// First line is "---", second line should start with "id:"
	if len(lines) < 2 {
		t.Fatal("too few lines")
	}
	if lines[0] != "---" {
		t.Errorf("expected first line '---', got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "id:") {
		t.Errorf("expected second line to start with 'id:', got %q", lines[1])
	}
}
