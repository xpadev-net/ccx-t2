package ledger

import (
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

	tasks, _ := l.Load()
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

	_ = l.Add(Task{ID: "task-001", Title: "T", Status: "unstarted"})
	tasks, _ := l.Load()
	firstUpdatedAt := tasks[0].UpdatedAt

	time.Sleep(1100 * time.Millisecond)
	if err := l.Update("task-001", map[string]any{"title": "Updated"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	tasks, _ = l.Load()
	if tasks[0].UpdatedAt == firstUpdatedAt {
		t.Error("updated_at not changed after Update")
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

	_ = l.Add(Task{ID: "task-001", Title: "T", Status: "unstarted"})
	err := l.Archive("task-001", "")
	if err == nil {
		t.Error("expected error archiving non-completed task")
	}
}

func TestArchiveMovesFile(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)

	_ = l.Add(Task{ID: "task-001", Title: "Do thing", Status: "completed", Body: "body"})
	if err := l.Archive("task-001", "abc123"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Task should be removed from ledger.
	tasks, _ := l.Load()
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
	_ = l.Add(Task{ID: "task-001", Title: "T", Status: "completed", Body: body})
	_ = l.Archive("task-001", "deadbeef")

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
		_ = l.Add(Task{ID: id, Title: "T", Status: "unstarted"})
	}
}

func TestGenerateIDNoConflictWithArchive(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	_ = os.MkdirAll(archiveDir, 0o755)

	date := time.Now().Format("20060102")
	existingID := "task-" + date + "-0001"
	// Create a fake archive file with this ID.
	_ = os.WriteFile(filepath.Join(archiveDir, existingID+"-some-slug.md"), []byte("content"), 0o644)

	l := NewLedger(filepath.Join(dir, "ledger.md"), archiveDir)
	id, err := l.GenerateID()
	if err != nil {
		t.Fatalf("GenerateID: %v", err)
	}
	if id == existingID {
		t.Errorf("GenerateID returned conflicting ID %s", id)
	}
}

// ---- FrontMatter field order test ----

func TestIDFirstInFrontMatter(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))

	_ = l.Add(Task{ID: "task-001", Title: "Test", Status: "unstarted"})

	data, _ := os.ReadFile(filepath.Join(dir, "ledger.md"))
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
