package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// ErrTaskExists reports an attempted insert for an existing task ID.
	ErrTaskExists = errors.New("task ID already exists")
	// ErrTaskNotFound reports a requested task ID that is not present.
	ErrTaskNotFound = errors.New("task not found")
	// ErrTaskDeleteInProgress reports a task that is already reserved for deletion.
	ErrTaskDeleteInProgress = errors.New("task delete already in progress")
)

// Task represents a single task in the ledger.
type Task struct {
	ID             string   `yaml:"id"              json:"id"`
	Title          string   `yaml:"title,omitempty" json:"title,omitempty"`
	Status         string   `yaml:"status,omitempty" json:"status,omitempty"`
	Branch         string   `yaml:"branch,omitempty" json:"branch,omitempty"`
	WorkerID       string   `yaml:"worker_id,omitempty" json:"worker_id,omitempty"`
	Harness        string   `yaml:"harness,omitempty" json:"harness,omitempty"`
	AllowedFiles   []string `yaml:"allowed_files,omitempty" json:"allowed_files,omitempty"`
	ForbiddenFiles []string `yaml:"forbidden_files,omitempty" json:"forbidden_files,omitempty"`
	PrURL          string   `yaml:"pr_url,omitempty" json:"pr_url,omitempty"`
	MergeCommit    string   `yaml:"merge_commit,omitempty" json:"merge_commit,omitempty"`
	Reason         string   `yaml:"reason,omitempty" json:"reason,omitempty"`
	UpdatedAt      string   `yaml:"updated_at,omitempty" json:"updated_at,omitempty"`

	// Body is the markdown body of the task (not part of front matter or JSON).
	Body string `yaml:"-" json:"-"`
}

// Ledger manages the task ledger file.
type Ledger struct {
	mu         sync.Mutex
	filePath   string
	archiveDir string
	// onChange is called whenever the ledger is modified.
	onChange func()
}

// NewLedger creates a new Ledger for the given file path.
// archiveDir is the directory where archived tasks are stored.
func NewLedger(filePath, archiveDir string) *Ledger {
	return &Ledger{
		filePath:   filePath,
		archiveDir: archiveDir,
	}
}

// SetOnChange registers a callback invoked after each successful write.
func (l *Ledger) SetOnChange(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onChange = fn
}

// Load reads and parses the ledger file.
func (l *Ledger) Load() ([]Task, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.load()
}

// LoadByID reads and returns a single task by ID.
func (l *Ledger) LoadByID(id string) (Task, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tasks, err := l.load()
	if err != nil {
		return Task{}, false, err
	}
	for _, task := range tasks {
		if task.ID == id {
			return task, true, nil
		}
	}
	return Task{}, false, nil
}

func (l *Ledger) load() ([]Task, error) {
	data, err := os.ReadFile(l.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseContent(string(data))
}

func parseContent(content string) ([]Task, error) {
	raws, err := parse(content)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(raws))
	for _, r := range raws {
		t, err := unmarshalTask(r.frontMatter)
		if err != nil {
			return nil, fmt.Errorf("parse task front matter: %w", err)
		}
		t.Body = strings.Trim(r.body, "\n")
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// Save writes tasks back to the ledger file atomically.
func (l *Ledger) Save(tasks []Task) error {
	l.mu.Lock()
	err := l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return err
}

func (l *Ledger) save(tasks []Task) error {
	var sb strings.Builder
	for _, t := range tasks {
		fm, err := marshalFrontMatter(t)
		if err != nil {
			return fmt.Errorf("marshal task %s: %w", t.ID, err)
		}
		sb.WriteString("---\n")
		sb.Write(fm)
		sb.WriteString("---\n")
		if t.Body != "" {
			sb.WriteString("\n")
			sb.WriteString(t.Body)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	dir := filepath.Dir(l.filePath)
	tmp, err := os.CreateTemp(dir, ".ledger-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.WriteString(sb.String()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, l.filePath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// AddNew generates a unique ID and appends the task atomically in a single
// read + write, returning the generated ID. Use this instead of separate
// GenerateID + Add calls to avoid a TOCTOU race where two concurrent callers
// observe the same on-disk state and generate the same sequence number.
func (l *Ledger) AddNew(task Task) (string, error) {
	l.mu.Lock()
	// Load once and reuse for both ID generation and the append.
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return "", err
	}
	id, err := l.generateIDFromTasks(tasks)
	if err != nil {
		l.mu.Unlock()
		return "", err
	}
	task.ID = id
	task.UpdatedAt = time.Now().Format(time.RFC3339)
	tasks = append(tasks, task)
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err != nil {
		// Return empty id on save failure so callers cannot reference an
		// ID that was never persisted to disk.
		return "", err
	}
	if onChange != nil {
		onChange()
	}
	return id, nil
}

// Add appends a new task to the ledger with updated_at set to now.
func (l *Ledger) Add(task Task) error {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return err
	}
	for _, t := range tasks {
		if t.ID == task.ID {
			l.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrTaskExists, task.ID)
		}
	}
	task.UpdatedAt = time.Now().Format(time.RFC3339)
	tasks = append(tasks, task)
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return err
}

// AddAll atomically appends multiple tasks in a single file write.
// All tasks get the same updated_at timestamp. This prevents partial
// child-task writes that would leave orphaned tasks on retry.
func (l *Ledger) AddAll(newTasks []Task) error {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return err
	}
	// Build a set of existing IDs (including IDs within newTasks) to detect duplicates.
	existing := make(map[string]bool, len(tasks)+len(newTasks))
	for _, t := range tasks {
		existing[t.ID] = true
	}
	now := time.Now().Format(time.RFC3339)
	stamped := make([]Task, len(newTasks))
	for i, t := range newTasks {
		if existing[t.ID] {
			l.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrTaskExists, t.ID)
		}
		existing[t.ID] = true
		t.UpdatedAt = now
		stamped[i] = t
	}
	tasks = append(tasks, stamped...)
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return err
}

// AddAllNew generates IDs and appends all tasks atomically under one lock.
// It returns the generated IDs in the same order as newTasks.
func (l *Ledger) AddAllNew(newTasks []Task) ([]string, error) {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	ids, err := l.generateIDsFromTasks(tasks, len(newTasks))
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	now := time.Now().Format(time.RFC3339)
	stamped := make([]Task, len(newTasks))
	for i, t := range newTasks {
		t.ID = ids[i]
		t.UpdatedAt = now
		stamped[i] = t
	}
	tasks = append(tasks, stamped...)
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if onChange != nil {
		onChange()
	}
	return ids, nil
}

// DeleteTasks removes tasks with the given IDs from the ledger, ignoring
// IDs that are not present. Used to roll back orphaned tasks after a
// partial write failure (e.g., AddAll succeeded but the parent Update failed).
func (l *Ledger) DeleteTasks(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return err
	}
	del := make(map[string]bool, len(ids))
	for _, id := range ids {
		del[id] = true
	}
	remaining := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		if !del[t.ID] {
			remaining = append(remaining, t)
		}
	}
	if len(remaining) == len(tasks) {
		// Nothing to delete.
		l.mu.Unlock()
		return nil
	}
	err = l.save(remaining)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return err
}

// DeleteTaskReturnPrev removes one task and returns the removed snapshot.
func (l *Ledger) DeleteTaskReturnPrev(id string) (Task, error) {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return Task{}, err
	}
	idx := -1
	var prev Task
	for i, task := range tasks {
		if task.ID == id {
			idx = i
			prev = task
			break
		}
	}
	if idx == -1 {
		l.mu.Unlock()
		return Task{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	remaining := make([]Task, 0, len(tasks)-1)
	remaining = append(remaining, tasks[:idx]...)
	remaining = append(remaining, tasks[idx+1:]...)
	err = l.save(remaining)
	onChange := l.onChange
	l.mu.Unlock()
	if err != nil {
		return Task{}, err
	}
	if onChange != nil {
		onChange()
	}
	return prev, nil
}

// PrepareDeleteReturnPrev either marks a task as deleting or removes it under
// one ledger lock. shouldMark receives the current snapshot; when it returns
// true the task remains in the ledger with status "deleting", marker contains
// that reserved row, and marked is true. When the task is already deleting and
// shouldMark returns false, the task is left unchanged and ErrTaskDeleteInProgress
// is returned. Otherwise the task is removed and marked is false.
func (l *Ledger) PrepareDeleteReturnPrev(id string, shouldMark func(Task) bool) (prev Task, marker Task, marked bool, err error) {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return Task{}, Task{}, false, err
	}
	idx := -1
	for i, task := range tasks {
		if task.ID == id {
			idx = i
			prev = task
			break
		}
	}
	if idx == -1 {
		l.mu.Unlock()
		return Task{}, Task{}, false, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	mark := shouldMark(prev)
	if prev.Status == "deleting" && !mark {
		l.mu.Unlock()
		return prev, prev, false, fmt.Errorf("%w: %s", ErrTaskDeleteInProgress, id)
	}
	if mark {
		tasks[idx].Status = "deleting"
		tasks[idx].UpdatedAt = time.Now().Format(time.RFC3339Nano)
		marker = tasks[idx]
		err = l.save(tasks)
		onChange := l.onChange
		l.mu.Unlock()
		if err != nil {
			return Task{}, Task{}, false, err
		}
		if onChange != nil {
			onChange()
		}
		return prev, marker, true, nil
	}
	remaining := make([]Task, 0, len(tasks)-1)
	remaining = append(remaining, tasks[:idx]...)
	remaining = append(remaining, tasks[idx+1:]...)
	err = l.save(remaining)
	onChange := l.onChange
	l.mu.Unlock()
	if err != nil {
		return Task{}, Task{}, false, err
	}
	if onChange != nil {
		onChange()
	}
	return prev, Task{}, false, nil
}

// DeleteTaskIfCurrent removes the task only when its current deleting marker
// still matches expected. It returns false when another caller already deleted
// or replaced the marker.
func (l *Ledger) DeleteTaskIfCurrent(expected Task) (bool, error) {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return false, err
	}
	idx := -1
	for i, task := range tasks {
		if task.ID == expected.ID {
			if !sameDeleteMarker(task, expected) {
				l.mu.Unlock()
				return false, nil
			}
			idx = i
			break
		}
	}
	if idx == -1 {
		l.mu.Unlock()
		return false, nil
	}
	remaining := make([]Task, 0, len(tasks)-1)
	remaining = append(remaining, tasks[:idx]...)
	remaining = append(remaining, tasks[idx+1:]...)
	err = l.save(remaining)
	onChange := l.onChange
	l.mu.Unlock()
	if err != nil {
		return false, err
	}
	if onChange != nil {
		onChange()
	}
	return true, nil
}

// RestoreTaskSnapshotIfCurrent replaces a matching deleting marker with the
// snapshot. It returns false when another caller already deleted or replaced the
// marker.
func (l *Ledger) RestoreTaskSnapshotIfCurrent(snapshot Task, expected Task) (bool, error) {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return false, err
	}
	for i, task := range tasks {
		if task.ID == expected.ID {
			if !sameDeleteMarker(task, expected) {
				l.mu.Unlock()
				return false, nil
			}
			tasks[i] = snapshot
			err = l.save(tasks)
			onChange := l.onChange
			l.mu.Unlock()
			if err != nil {
				return false, err
			}
			if onChange != nil {
				onChange()
			}
			return true, nil
		}
	}
	l.mu.Unlock()
	return false, nil
}

func sameDeleteMarker(a, b Task) bool {
	return a.ID == b.ID &&
		a.Status == "deleting" &&
		b.Status == "deleting" &&
		a.UpdatedAt == b.UpdatedAt
}

// RestoreTaskSnapshot replaces an existing task with the snapshot, or appends
// it when the ID is currently missing. The snapshot is written as-is, including
// UpdatedAt.
func (l *Ledger) RestoreTaskSnapshot(task Task) error {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return err
	}
	replaced := false
	for i := range tasks {
		if tasks[i].ID == task.ID {
			tasks[i] = task
			replaced = true
			break
		}
	}
	if !replaced {
		tasks = append(tasks, task)
	}
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err != nil {
		return err
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// UpdateIfStatus modifies task fields only when the task's current status
// matches expectedStatus. Returns an error if the status has changed (e.g.,
// due to a concurrent split_task or stop_worker call). Use this for transitions
// that require the task to still be in a specific state, such as spawn_worker
// moving a task from "unstarted" to "in_progress".
func (l *Ledger) UpdateIfStatus(id, expectedStatus string, fields map[string]any) error {
	return l.updateWithCheck(id, expectedStatus, fields)
}

// UpdateIfStatuses modifies task fields only when the task's current status is
// in the allowedStatuses list. Returns an error if the current status is not
// in the allowed set.
func (l *Ledger) UpdateIfStatuses(id string, allowedStatuses []string, fields map[string]any) error {
	_, err := l.UpdateIfStatusesReturnPrev(id, allowedStatuses, fields)
	return err
}

// UpdateIfStatusesReturnPrev modifies task fields only when the current status
// is in the allowed set, and returns the pre-update snapshot under the same lock.
func (l *Ledger) UpdateIfStatusesReturnPrev(id string, allowedStatuses []string, fields map[string]any) (prev Task, err error) {
	return l.UpdateIfStatusesReturnPrevWith(id, allowedStatuses, func(current Task) (map[string]any, error) {
		return fields, nil
	})
}

// UpdateIfStatusesReturnPrevWith computes and applies task fields only when the
// current status is in the allowed set. The updater receives the current task
// snapshot under the ledger lock, and the method returns that same pre-update
// snapshot.
func (l *Ledger) UpdateIfStatusesReturnPrevWith(id string, allowedStatuses []string, updater func(Task) (map[string]any, error)) (prev Task, err error) {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return Task{}, err
	}
	found := false
	for i := range tasks {
		if tasks[i].ID != id {
			continue
		}
		found = true
		current := tasks[i].Status
		allowed := false
		for _, s := range allowedStatuses {
			if s == current {
				allowed = true
				break
			}
		}
		if !allowed {
			l.mu.Unlock()
			return Task{}, fmt.Errorf("task %s status is %q, not in allowed set %v", id, current, allowedStatuses)
		}
		prev = tasks[i]
		fields, err := updater(prev)
		if err != nil {
			l.mu.Unlock()
			return Task{}, err
		}
		t := &tasks[i]
		if err := applyFields(t, fields); err != nil {
			l.mu.Unlock()
			return Task{}, err
		}
		t.UpdatedAt = time.Now().Format(time.RFC3339)
		break
	}
	if !found {
		l.mu.Unlock()
		return Task{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return prev, err
}

// UpdateReturnPrev applies the field update and returns a snapshot of the task
// from before the write, all within a single mutex lock. The caller can compare
// the returned prev task with its earlier read to detect concurrent transitions
// (e.g., a spawned worker that changed status between the caller's Load and this Update).
func (l *Ledger) UpdateReturnPrev(id string, fields map[string]any) (prev Task, err error) {
	return l.UpdateReturnPrevWith(id, func(Task) (map[string]any, error) {
		return fields, nil
	})
}

// UpdateReturnPrevWith computes and applies task fields while holding the
// ledger lock, and returns the pre-update snapshot.
func (l *Ledger) UpdateReturnPrevWith(id string, updater func(Task) (map[string]any, error)) (prev Task, err error) {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return Task{}, err
	}
	found := false
	for i := range tasks {
		if tasks[i].ID != id {
			continue
		}
		prev = tasks[i] // snapshot before mutation
		found = true
		fields, err := updater(prev)
		if err != nil {
			l.mu.Unlock()
			return Task{}, err
		}
		t := &tasks[i]
		if err := applyFields(t, fields); err != nil {
			l.mu.Unlock()
			return Task{}, err
		}
		t.UpdatedAt = time.Now().Format(time.RFC3339)
		break
	}
	if !found {
		l.mu.Unlock()
		return Task{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return prev, err
}

func (l *Ledger) updateWithCheck(id, expectedStatus string, fields map[string]any) error {
	l.mu.Lock()
	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return err
	}
	found := false
	for i := range tasks {
		if tasks[i].ID != id {
			continue
		}
		if tasks[i].Status != expectedStatus {
			l.mu.Unlock()
			return fmt.Errorf("task %s status is %q, expected %q (concurrent modification?)",
				id, tasks[i].Status, expectedStatus)
		}
		found = true
		t := &tasks[i]
		if err := applyFields(t, fields); err != nil {
			l.mu.Unlock()
			return err
		}
		t.UpdatedAt = time.Now().Format(time.RFC3339)
		break
	}
	if !found {
		l.mu.Unlock()
		return fmt.Errorf("task not found: %s", id)
	}
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return err
}

// applyFields updates task fields from the given map (same semantics as Update).
func applyFields(t *Task, fields map[string]any) error {
	for k, v := range fields {
		switch k {
		case "title":
			if s, ok := v.(string); ok {
				t.Title = s
			}
		case "status":
			if s, ok := v.(string); ok {
				t.Status = s
			}
		case "branch":
			if s, ok := v.(string); ok {
				t.Branch = s
			}
		case "worker_id":
			if s, ok := v.(string); ok {
				t.WorkerID = s
			}
		case "harness":
			if s, ok := v.(string); ok {
				t.Harness = s
			}
		case "allowed_files":
			switch val := v.(type) {
			case []string:
				t.AllowedFiles = val
			case nil:
				t.AllowedFiles = nil
			}
		case "forbidden_files":
			switch val := v.(type) {
			case []string:
				t.ForbiddenFiles = val
			case nil:
				t.ForbiddenFiles = nil
			}
		case "pr_url":
			if s, ok := v.(string); ok {
				t.PrURL = s
			}
		case "merge_commit":
			if s, ok := v.(string); ok {
				t.MergeCommit = s
			}
		case "reason":
			if s, ok := v.(string); ok {
				t.Reason = s
			}
		case "body":
			if s, ok := v.(string); ok {
				t.Body = s
			}
		}
	}
	return nil
}

// Update modifies fields of an existing task by ID, updating updated_at.
func (l *Ledger) Update(id string, fields map[string]any) error {
	l.mu.Lock()

	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return err
	}

	found := false
	for i := range tasks {
		if tasks[i].ID != id {
			continue
		}
		found = true
		t := &tasks[i]
		if err := applyFields(t, fields); err != nil {
			l.mu.Unlock()
			return err
		}
		t.UpdatedAt = time.Now().Format(time.RFC3339)
		break
	}

	if !found {
		l.mu.Unlock()
		return fmt.Errorf("task not found: %s", id)
	}
	err = l.save(tasks)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return err
}

// Archive moves a completed task to the archive directory.
// mergeCommit is optional; if non-empty it's added to the archive front matter.
func (l *Ledger) Archive(id, mergeCommit string) error {
	l.mu.Lock()

	tasks, err := l.load()
	if err != nil {
		l.mu.Unlock()
		return err
	}

	idx := -1
	for i, t := range tasks {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		archived, err := l.archiveContainsID(id)
		if err != nil {
			l.mu.Unlock()
			return err
		}
		if archived {
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		return fmt.Errorf("task not found: %s", id)
	}

	t := tasks[idx]
	if t.Status != "completed" {
		l.mu.Unlock()
		return fmt.Errorf("task %s is not completed (status: %s)", id, t.Status)
	}

	slug := titleToSlug(t.Title)
	t.Body = removeMergeCommitComment(t.Body)
	if mergeCommit != "" {
		t.MergeCommit = mergeCommit
	}

	archiveFile := filepath.Join(l.archiveDir, fmt.Sprintf("%s-%s.md", id, slug))
	if err := os.MkdirAll(l.archiveDir, 0o755); err != nil {
		l.mu.Unlock()
		return err
	}

	// If the archive file already exists we crashed between the last archive
	// write and the ledger save; skip re-writing and proceed to remove the
	// task from the ledger (idempotent recovery).
	_, statErr := os.Stat(archiveFile)
	if statErr != nil && !os.IsNotExist(statErr) {
		l.mu.Unlock()
		return fmt.Errorf("stat archive file: %w", statErr)
	}
	if os.IsNotExist(statErr) {
		fm, err := marshalFrontMatter(t)
		if err != nil {
			l.mu.Unlock()
			return err
		}
		var ab strings.Builder
		ab.WriteString("---\n")
		ab.Write(fm)
		ab.WriteString("---\n")
		if t.Body != "" {
			ab.WriteString("\n")
			ab.WriteString(t.Body)
			ab.WriteString("\n")
		}

		// Write archive file atomically via temp file + rename.
		tmpArchive, err := os.CreateTemp(l.archiveDir, ".archive-*.tmp")
		if err != nil {
			l.mu.Unlock()
			return err
		}
		tmpArchiveName := tmpArchive.Name()
		if _, err := tmpArchive.WriteString(ab.String()); err != nil {
			tmpArchive.Close()
			os.Remove(tmpArchiveName)
			l.mu.Unlock()
			return err
		}
		if err := tmpArchive.Close(); err != nil {
			os.Remove(tmpArchiveName)
			l.mu.Unlock()
			return err
		}
		if err := os.Rename(tmpArchiveName, archiveFile); err != nil {
			os.Remove(tmpArchiveName)
			l.mu.Unlock()
			return err
		}
	}

	// Build remaining slice without aliasing the original backing array.
	remaining := make([]Task, 0, len(tasks)-1)
	remaining = append(remaining, tasks[:idx]...)
	remaining = append(remaining, tasks[idx+1:]...)

	err = l.save(remaining)
	onChange := l.onChange
	l.mu.Unlock()
	if err == nil && onChange != nil {
		onChange()
	}
	return err
}

// IsArchived reports whether an archive file for id already exists.
func (l *Ledger) IsArchived(id string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.archiveContainsID(id)
}

// ArchivedTask returns the archived task metadata for id if it exists.
func (l *Ledger) ArchivedTask(id string) (Task, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.archivedTask(id)
}

// GenerateID generates a unique task ID in the format task-{YYYYMMDD}-{4-digit seq}.
func (l *Ledger) GenerateID() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generateID()
}

// GenerateIDs generates n unique task IDs in a single read pass.
// All IDs are distinct from each other and from existing ledger/archive IDs.
// Callers generating IDs for multiple tasks must use this method instead of
// calling GenerateID in a loop, which would return the same ID each time
// (since the IDs are not on-disk yet when the next call reads the ledger).
func (l *Ledger) GenerateIDs(n int) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generateIDs(n)
}

func (l *Ledger) generateIDs(n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	tasks, err := l.load()
	if err != nil {
		return nil, err
	}
	return l.generateIDsFromTasks(tasks, n)
}

func (l *Ledger) generateIDsFromTasks(tasks []Task, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	existing := make(map[string]bool)
	for _, t := range tasks {
		existing[t.ID] = true
	}
	archiveIDs, err := l.archiveIDs()
	if err != nil {
		return nil, err
	}
	for id := range archiveIDs {
		existing[id] = true
	}

	date := time.Now().Format("20060102")
	ids := make([]string, 0, n)
	for seq := 1; seq <= 9999 && len(ids) < n; seq++ {
		id := fmt.Sprintf("task-%s-%04d", date, seq)
		if !existing[id] {
			ids = append(ids, id)
			existing[id] = true // reserve for subsequent iterations
		}
	}
	if len(ids) < n {
		return nil, fmt.Errorf("could not generate %d unique task IDs for date %s", n, date)
	}
	return ids, nil
}

func (l *Ledger) generateID() (string, error) {
	tasks, err := l.load()
	if err != nil {
		return "", err
	}
	return l.generateIDFromTasks(tasks)
}

func (l *Ledger) generateIDFromTasks(tasks []Task) (string, error) {
	existing := make(map[string]bool)
	for _, t := range tasks {
		existing[t.ID] = true
	}
	archiveIDs, err := l.archiveIDs()
	if err != nil {
		return "", err
	}
	for id := range archiveIDs {
		existing[id] = true
	}

	date := time.Now().Format("20060102")
	for seq := 1; seq <= 9999; seq++ {
		id := fmt.Sprintf("task-%s-%04d", date, seq)
		if !existing[id] {
			return id, nil
		}
	}
	return "", fmt.Errorf("could not generate unique task ID for date %s", date)
}

func (l *Ledger) archiveContainsID(id string) (bool, error) {
	ids, err := l.archiveIDs()
	if err != nil {
		return false, err
	}
	return ids[id], nil
}

func (l *Ledger) archiveIDs() (map[string]bool, error) {
	ids := make(map[string]bool)
	entries, err := os.ReadDir(l.archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ids, nil
		}
		return nil, fmt.Errorf("read archive directory: %w", err)
	}
	for _, e := range entries {
		if id, ok := archiveIDFromFilename(e.Name()); ok {
			ids[id] = true
		}
	}
	return ids, nil
}

func (l *Ledger) archivedTask(id string) (Task, bool, error) {
	entries, err := os.ReadDir(l.archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return Task{}, false, nil
		}
		return Task{}, false, fmt.Errorf("read archive directory: %w", err)
	}
	for _, e := range entries {
		if archiveID, ok := archiveIDFromFilename(e.Name()); !ok || archiveID != id {
			continue
		}
		data, err := os.ReadFile(filepath.Join(l.archiveDir, e.Name()))
		if err != nil {
			return Task{}, false, fmt.Errorf("read archive task %s: %w", id, err)
		}
		tasks, err := parseContent(string(data))
		if err != nil {
			return Task{}, false, fmt.Errorf("parse archive task %s: %w", id, err)
		}
		if len(tasks) == 0 {
			return Task{}, false, fmt.Errorf("archive task %s has no task content", id)
		}
		return tasks[0], true, nil
	}
	return Task{}, false, nil
}

func archiveIDFromFilename(name string) (string, bool) {
	if !strings.HasSuffix(name, ".md") {
		return "", false
	}
	stem := strings.TrimSuffix(name, ".md")
	if match := reTaskID.FindString(stem); match != "" && (len(stem) == len(match) || stem[len(match)] == '-') {
		return match, true
	}
	// Compatibility for legacy tests/fixtures that use task-NNN IDs.
	if match := reLegacyTaskID.FindString(stem); match != "" && (len(stem) == len(match) || stem[len(match)] == '-') {
		return match, true
	}
	return "", false
}

var (
	reSlugNonAlnum = regexp.MustCompile(`[^a-z0-9-]`)
	reSlugHyphens  = regexp.MustCompile(`-{2,}`)
	reMergeCommit  = regexp.MustCompile(`(?m)\n?<!-- merge_commit: [^\n]+ -->`)
	reTaskID       = regexp.MustCompile(`^task-\d{8}-\d{4}`)
	reLegacyTaskID = regexp.MustCompile(`^task-\d{3}`)
)

// titleToSlug converts a title to a URL slug.
func titleToSlug(title string) string {
	s := strings.ToLower(title)
	s = strings.ReplaceAll(s, " ", "-")
	s = reSlugNonAlnum.ReplaceAllString(s, "")
	s = reSlugHyphens.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "untitled"
	}
	return s
}

// removeMergeCommitComment strips "<!-- merge_commit: ... -->" from body text.
func removeMergeCommitComment(body string) string {
	return reMergeCommit.ReplaceAllString(body, "")
}

// RemoveMergeCommitComments strips internal merge_commit markers from task body
// text before a fresh verified marker is appended.
func RemoveMergeCommitComments(body string) string {
	return removeMergeCommitComment(body)
}
