package delegation

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
)

// ErrJobNotFound reports a delegation id this service does not own.
var ErrJobNotFound = errors.New("delegation: job not found")

// Job is one asynchronous delegation. The ID is also the child session id, so
// a status lookup and an answer address the same child.
type Job struct {
	ID     string
	Target string
	Mode   Mode
	Status Status
	Input  string

	ParentSessionID string
	ParentAgent     string
	ParentTaskID    string
	CallID          string
	Depth           int
	ChainID         string
	ChildSessionID  string

	Content  message.Content
	Question *message.Content
	Error    string

	// TraceParent is the W3C traceparent of the turn that created this job. The
	// job outlives that turn, so the context cannot: this is what lets the child's
	// run join the trace it came from.
	TraceParent string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Clone deep-copies the payload a reader can observe.
func (j Job) Clone() Job {
	j.Content = j.Content.Clone()
	if j.Question != nil {
		question := j.Question.Clone()
		j.Question = &question
	}
	return j
}

// Store persists asynchronous jobs. The first implementation is in-memory;
// a durable one can add leases without changing this interface.
type Store interface {
	Create(ctx context.Context, job Job) (created Job, found bool, err error)
	Get(ctx context.Context, id string) (Job, error)
	Update(ctx context.Context, id string, mutate func(*Job)) error
}

// MemStore is the single-process store.
type MemStore struct {
	mu   sync.Mutex
	jobs map[string]Job
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{jobs: make(map[string]Job)}
}

func (s *MemStore) Create(_ context.Context, job Job) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.jobs[job.ID]; ok {
		return existing.Clone(), true, nil
	}
	s.jobs[job.ID] = job.Clone()
	return job.Clone(), false, nil
}

func (s *MemStore) Get(_ context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrJobNotFound
	}
	return job.Clone(), nil
}

func (s *MemStore) Update(_ context.Context, id string, mutate func(*Job)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	mutate(&job)
	job.UpdatedAt = time.Now()
	s.jobs[id] = job.Clone()
	return nil
}
