package workflow

import "time"

// Task represents a tracked unit of work managed by a TaskManager.
type Task struct {
	ID        string     `json:"id"`
	Status    TaskStatus `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	Result    *Result    `json:"result,omitempty"`
}

// TaskManager is an optional capability for run/task orchestration (list/cancel).
// Default Runtime implementations may return a no-op or nil adapter.
type TaskManager interface {
	GetTask(id string) (*Task, error)
	CancelTask(id string) error
	ListTasks() ([]*Task, error)
}
