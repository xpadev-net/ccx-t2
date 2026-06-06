package worker

import (
	"sync"
	"time"
)

// Info holds runtime metadata for an active Worker.
type Info struct {
	TaskID    string    `json:"task_id"`
	WorkerID  string    `json:"worker_id"`
	Harness   string    `json:"harness"`
	StartedAt time.Time `json:"started_at"`
}

// Registry is an in-memory store of active Workers keyed by worker_id.
type Registry struct {
	mu      sync.RWMutex
	workers map[string]Info
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{workers: make(map[string]Info)}
}

// Register adds or replaces a Worker entry.
func (r *Registry) Register(info Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers[info.WorkerID] = info
}

// Remove deletes a Worker entry by worker_id.
func (r *Registry) Remove(workerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.workers, workerID)
}

// Get returns the Worker info for the given worker_id, if present.
func (r *Registry) Get(workerID string) (Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.workers[workerID]
	return info, ok
}

// All returns a snapshot of all registered Workers.
func (r *Registry) All() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.workers))
	for _, info := range r.workers {
		out = append(out, info)
	}
	return out
}

// FindByTaskID returns the Worker info for the given task_id, if present.
func (r *Registry) FindByTaskID(taskID string) (Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, info := range r.workers {
		if info.TaskID == taskID {
			return info, true
		}
	}
	return Info{}, false
}
