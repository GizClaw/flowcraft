package ltm

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// -----------------------------------------------------------------------------
// Public types
// -----------------------------------------------------------------------------

// JobID uniquely identifies an async Save. Backed by ULID so
// lexicographic ordering matches creation time across processes.
type JobID string

// JobState enumerates the lifecycle states for an async Save job.
type JobState string

const (
	JobPending   JobState = "pending"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobDead      JobState = "dead"
)

// JobStatus is the public status of a SaveAsync job.
type JobStatus struct {
	ID        JobID
	State     JobState
	Attempts  int
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
	EntryIDs  []string
}

// JobPayload is the durable representation of a Save invocation.
type JobPayload struct {
	Scope    MemoryScope   `json:"scope"`
	Messages []llm.Message `json:"messages"`
}

// JobRecord is the persisted view of one async Save.
type JobRecord struct {
	ID        JobID      `json:"id"`
	Namespace string     `json:"namespace"`
	Payload   JobPayload `json:"payload"`
	State     JobState   `json:"state"`
	Attempts  int        `json:"attempts"`
	LastError string     `json:"last_error,omitempty"`
	EntryIDs  []string   `json:"entry_ids,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	NextRunAt time.Time  `json:"next_run_at"`
}

// JobQueue is the persistence layer for async Save jobs.
//
// Implementations must be safe for concurrent use by N workers.
//
// at-least-once: a Job that crashes after Lease but before Complete will be
// re-leased after its NextRunAt expires; idempotency is achieved at the Index
// layer via deterministic doc IDs.
type JobQueue interface {
	Enqueue(ctx context.Context, namespace string, payload JobPayload) (JobID, error)
	Lease(ctx context.Context, now time.Time) (*JobRecord, bool, error)
	Reschedule(ctx context.Context, id JobID, nextRun time.Time, lastErr string) error
	Complete(ctx context.Context, id JobID, entryIDs []string) error
	Fail(ctx context.Context, id JobID, lastErr string) error
	Get(ctx context.Context, id JobID) (*JobRecord, error)
	Close() error
}

func statusFromRecord(r *JobRecord) JobStatus {
	return JobStatus{
		ID:        r.ID,
		State:     r.State,
		Attempts:  r.Attempts,
		LastError: r.LastError,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
		EntryIDs:  append([]string(nil), r.EntryIDs...),
	}
}

// -----------------------------------------------------------------------------
// MemoryJobQueue — default in-process implementation
// -----------------------------------------------------------------------------

// MemoryJobQueue is the default in-process JobQueue.
//
// It does NOT survive a process crash. For crash-recoverable Async Save use
// sdkx/memory/jobqueue/sqlite.SQLiteJobQueue.
type MemoryJobQueue struct {
	mu     sync.Mutex
	jobs   map[JobID]*JobRecord
	leased map[JobID]struct{}
}

// NewMemoryJobQueue constructs an empty in-memory JobQueue.
func NewMemoryJobQueue() *MemoryJobQueue {
	return &MemoryJobQueue{jobs: map[JobID]*JobRecord{}, leased: map[JobID]struct{}{}}
}

// Close implements JobQueue.
func (q *MemoryJobQueue) Close() error { return nil }

// Enqueue implements JobQueue.
func (q *MemoryJobQueue) Enqueue(_ context.Context, ns string, p JobPayload) (JobID, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	id := NewJobID()
	now := time.Now()
	q.jobs[id] = &JobRecord{
		ID: id, Namespace: ns, Payload: p,
		State: JobPending, Attempts: 0,
		CreatedAt: now, UpdatedAt: now, NextRunAt: now,
	}
	return id, nil
}

// Lease atomically picks the oldest pending job with NextRunAt <= now.
func (q *MemoryJobQueue) Lease(_ context.Context, now time.Time) (*JobRecord, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var best *JobRecord
	for _, j := range q.jobs {
		if j.State != JobPending {
			continue
		}
		if j.NextRunAt.After(now) {
			continue
		}
		if best == nil || j.CreatedAt.Before(best.CreatedAt) {
			best = j
		}
	}
	if best == nil {
		return nil, false, nil
	}
	best.State = JobRunning
	best.Attempts++
	best.UpdatedAt = now
	q.leased[best.ID] = struct{}{}
	cp := *best
	return &cp, true, nil
}

// Reschedule pushes a leased job back to pending with a new NextRunAt.
func (q *MemoryJobQueue) Reschedule(_ context.Context, id JobID, next time.Time, lastErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	j.State = JobPending
	j.NextRunAt = next
	j.LastError = lastErr
	j.UpdatedAt = time.Now()
	delete(q.leased, id)
	return nil
}

// Complete marks a job succeeded.
func (q *MemoryJobQueue) Complete(_ context.Context, id JobID, entryIDs []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	j.State = JobSucceeded
	j.EntryIDs = append([]string(nil), entryIDs...)
	j.UpdatedAt = time.Now()
	delete(q.leased, id)
	return nil
}

// Fail marks a job dead (max attempts exhausted).
func (q *MemoryJobQueue) Fail(_ context.Context, id JobID, lastErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	j.State = JobDead
	j.LastError = lastErr
	j.UpdatedAt = time.Now()
	delete(q.leased, id)
	return nil
}

// Get implements JobQueue.
func (q *MemoryJobQueue) Get(_ context.Context, id JobID) (*JobRecord, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return nil, ErrJobNotFound
	}
	cp := *j
	return &cp, nil
}

// PendingCount is exposed for telemetry (memory.job.queue_depth gauge).
func (q *MemoryJobQueue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, j := range q.jobs {
		if j.State == JobPending {
			n++
		}
	}
	return n
}

// AllIDs is exposed for tests / introspection. The slice is sorted so callers
// get a deterministic ordering across runs.
func (q *MemoryJobQueue) AllIDs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := make([]string, 0, len(q.jobs))
	for id := range q.jobs {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	return ids
}

// -----------------------------------------------------------------------------
// Worker loop — owned by *lt
// -----------------------------------------------------------------------------

func (m *lt) worker() {
	defer m.wgWorkers.Done()
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}
		ctx := context.Background()
		rec, ok, err := m.cfg.JobQueue.Lease(ctx, m.cfg.Now())
		if err != nil {
			m.log("ltm.worker.lease: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !ok {
			select {
			case <-m.stopCh:
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		m.handleJob(ctx, rec)
	}
}

func (m *lt) handleJob(ctx context.Context, rec *JobRecord) {
	var extractOpts []ExtractOption
	if m.cfg.SaveWithContext {
		if existing := m.gatherExistingFacts(ctx, rec.Payload.Scope, rec.Payload.Messages); len(existing) > 0 {
			extractOpts = append(extractOpts, WithExistingFacts(existing))
		}
	}
	facts, err := m.cfg.Extractor.Extract(ctx, rec.Payload.Scope, rec.Payload.Messages, extractOpts...)
	if err != nil {
		m.failOrRetry(ctx, rec, err)
		return
	}
	ids, err := m.upsertFacts(ctx, rec.Payload.Scope, rec.Payload.Messages, facts, m.cfg.Now())
	if err != nil {
		m.failOrRetry(ctx, rec, err)
		return
	}
	if err := m.cfg.JobQueue.Complete(ctx, rec.ID, ids); err != nil {
		m.log("ltm.worker.complete: %v", err)
	}
}

func (m *lt) failOrRetry(ctx context.Context, rec *JobRecord, err error) {
	if rec.Attempts >= m.cfg.JobMaxAttempts {
		_ = m.cfg.JobQueue.Fail(ctx, rec.ID, err.Error())
		m.log("ltm.worker.dead %s: %v", rec.ID, err)
		return
	}
	d := m.cfg.JobBackoffBase
	for i := 1; i < rec.Attempts; i++ {
		d *= 2
		if d >= m.cfg.JobBackoffMax {
			d = m.cfg.JobBackoffMax
			break
		}
	}
	if d > m.cfg.JobBackoffMax {
		d = m.cfg.JobBackoffMax
	}
	next := m.cfg.Now().Add(d)
	_ = m.cfg.JobQueue.Reschedule(ctx, rec.ID, next, err.Error())
}
