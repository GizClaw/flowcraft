package recall

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/internal/background"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
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
	JobLeased    JobState = "leased"
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
	Scope    Scope         `json:"scope"`
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
	Start(ctx context.Context, id JobID, now time.Time) (*JobRecord, error)
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
// It does NOT survive a process crash. For crash-recoverable Async
// Save, plug a durable JobQueue adapter (e.g. SQLite-backed) supplied
// by an external package.
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
	best.State = JobLeased
	best.UpdatedAt = now
	q.leased[best.ID] = struct{}{}
	cp := *best
	return &cp, true, nil
}

// Start marks a leased job as running and consumes one attempt.
func (q *MemoryJobQueue) Start(ctx context.Context, id JobID, now time.Time) (*JobRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return nil, ErrJobNotFound
	}
	if j.State != JobLeased && j.State != JobRunning {
		return nil, errors.New("ltm: job is not leased")
	}
	if j.State == JobLeased {
		j.State = JobRunning
		j.Attempts++
		j.UpdatedAt = now
	}
	cp := *j
	return &cp, nil
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

// bookkeepingTimeout caps how long we wait for jobqueue accounting
// (Complete / Fail / Reschedule) to land after a job finishes or its
// per-job context is canceled. These calls run on a fresh ctx — derived
// from context.Background, NOT workerCtx — so Close()'s cancellation
// of workerCtx does not abort the very write that records the job's
// outcome (otherwise a leased job could stay "running" forever).
const bookkeepingTimeout = 5 * time.Second

func (m *lt) worker() {
	defer m.wgWorkers.Done()
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}
		// Lease is short and uses workerCtx so Close() unblocks any
		// blocking SQL Lease implementations promptly.
		rec, ok, err := m.cfg.jobQueue.Lease(m.workerCtx, m.cfg.now())
		if err != nil {
			// Treat ctx.Canceled (Close) as a clean exit signal, not a
			// queue error worth logging.
			if m.workerCtx.Err() != nil {
				return
			}
			m.log("ltm.worker.lease: %v", err)
			jobLeaseErrors.Add(m.workerCtx, 1)
			telemetry.Warn(m.workerCtx, "recall: worker lease failed",
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			select {
			case <-m.stopCh:
				return
			case <-time.After(100 * time.Millisecond):
			}
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
		if m.workerCtx.Err() != nil || m.stopping() {
			m.releaseLeasedJob(rec)
			return
		}
		started, err := m.cfg.jobQueue.Start(m.workerCtx, rec.ID, m.cfg.now())
		if err != nil {
			if m.workerCtx.Err() != nil || m.stopping() {
				m.releaseLeasedJob(rec)
				return
			}
			m.log("ltm.worker.start: %v", err)
			m.releaseLeasedJob(rec)
			select {
			case <-m.stopCh:
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		m.handleJob(started)
	}
}

func (m *lt) stopping() bool {
	select {
	case <-m.stopCh:
		return true
	default:
		return false
	}
}

func (m *lt) releaseLeasedJob(rec *JobRecord) {
	if rec == nil {
		return
	}
	bookCtx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	if err := m.cfg.jobQueue.Reschedule(bookCtx, rec.ID, m.cfg.now(), rec.LastError); err != nil {
		m.log("ltm.worker.release: %v", err)
	}
}

// handleJob runs one leased job under a per-job context derived from
// workerCtx with the configured timeout. The durable validate →
// extract → upsert portion goes through the shared [executeWrite]
// pipeline so the async path cannot drift from the sync ([Save]) one.
//
// Failure routing has three modes:
//
//  1. [writeStageError.Permanent] (validate stage today) → the
//     payload will never succeed under the current policy. Mark Fail
//     immediately so the queue does not waste retries on a payload
//     that was, e.g., enqueued before requireUserID was tightened
//     (issue #165) or originated from a direct queue Enqueue that
//     bypassed SaveAsync.
//  2. extractor / upsert returned a transient error → failOrRetry as
//     before. Includes ctx.Err() flowing through extractor on timeout
//     / Close, which is rescheduled with a short backoff so a
//     transient cancel does not advance attempts beyond the cap.
//  3. success → append history on a fresh bookkeeping ctx, then
//     Complete. Both bookkeeping calls deliberately run on a fresh
//     ctx (not jobCtx) so a workerCtx cancel during this last-mile
//     window does not skip the history append (issue #149) or orphan
//     the job in 'running'.
func (m *lt) handleJob(rec *JobRecord) {
	jobCtx, cancel := context.WithTimeout(m.workerCtx, m.cfg.jobTimeout)
	defer cancel()

	t0 := time.Now()
	defer func() {
		jobDuration.Record(jobCtx, time.Since(t0).Seconds())
	}()

	ids, _, err := m.executeWrite(jobCtx, rec.Payload.Scope, rec.Payload.Messages, m.cfg.now())
	if err != nil {
		stage := classifyWriteStage(err)
		m.recordJobFailure(jobCtx, rec, err, stage)
		if isPermanentWriteError(err) {
			// Skip retry — a permanent stage failure (validate)
			// will never succeed under the current policy.
			bookCtx, bookCancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
			defer bookCancel()
			if ferr := m.cfg.jobQueue.Fail(bookCtx, rec.ID, err.Error()); ferr != nil {
				m.log("ltm.worker.fail: %v", ferr)
			}
			return
		}
		m.failOrRetry(rec, err)
		return
	}
	ns := NamespaceFor(rec.Payload.Scope)
	// Append history AFTER persistence and BEFORE Complete. Uses an
	// independent short-timeout bookkeeping ctx so a workerCtx cancel
	// during this last-mile write does not skip the append, and the
	// append's own latency does not steal budget from Complete below.
	// Mirrors sync Save's "history is a quality booster, not part of
	// the durability contract" posture (errors are logged-not-raised
	// inside appendHistory). Fixes issue #149.
	if m.cfg.historyStore != nil {
		histCtx, histCancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
		m.appendHistory(histCtx, ns, rec.Payload.Messages)
		histCancel()
	}
	// Bookkeeping uses a fresh ctx so a workerCtx cancel during this
	// last-mile write does not orphan a successful job in 'running'.
	bookCtx, bookCancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer bookCancel()
	if err := m.cfg.jobQueue.Complete(bookCtx, rec.ID, ids); err != nil {
		m.log("ltm.worker.complete: %v", err)
		telemetry.Warn(bookCtx, "recall: job complete bookkeeping failed",
			otellog.String("job_id", string(rec.ID)),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}
	jobTotal.Add(jobCtx, 1, metric.WithAttributes(
		attribute.String("outcome", "succeeded"),
		attribute.Int("attempts", rec.Attempts),
	))
}

// classifyWriteStage extracts the phase tag from a writeStageError
// for telemetry attribution. Returns "unknown" when the failure did
// not originate from executeWrite — defensive default so the metric
// dimension stays well-typed.
func classifyWriteStage(err error) string {
	var werr *writeStageError
	if errors.As(err, &werr) {
		return werr.Stage()
	}
	return "unknown"
}

// isPermanentWriteError reports whether the failure is a permanent
// stage failure (currently only the validate stage). Permanent
// failures must skip the retry loop — repeating the same JobPayload
// through the same gate produces the same rejection.
func isPermanentWriteError(err error) bool {
	var werr *writeStageError
	if errors.As(err, &werr) {
		return werr.Permanent()
	}
	return false
}

// recordJobFailure emits the OTel signal for a failed handleJob attempt.
// Timeout is split out from generic error so dashboards can isolate
// "extractor wedged" from "extractor returned error". Job retry/dead
// transitions are still owned by failOrRetry; this only annotates the
// observation.
func (m *lt) recordJobFailure(ctx context.Context, rec *JobRecord, err error, stage string) {
	outcome := jobFailureOutcome(err)
	jobTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("stage", stage),
		attribute.Int("attempts", rec.Attempts),
	))
	telemetry.Warn(ctx, "recall: async job failed",
		otellog.String("job_id", string(rec.ID)),
		otellog.String("stage", stage),
		otellog.String("outcome", outcome),
		otellog.Int("attempts", rec.Attempts),
		otellog.String(telemetry.AttrErrorMessage, err.Error()))
}

func jobFailureOutcome(err error) string {
	return background.Classify(err).String()
}

func (m *lt) failOrRetry(rec *JobRecord, err error) {
	bookCtx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	if rec.Attempts >= m.cfg.jobMaxAttempts {
		_ = m.cfg.jobQueue.Fail(bookCtx, rec.ID, err.Error())
		m.log("ltm.worker.dead %s: %v", rec.ID, err)
		jobTotal.Add(bookCtx, 1, metric.WithAttributes(
			attribute.String("outcome", "dead"),
			attribute.Int("attempts", rec.Attempts),
		))
		telemetry.Warn(bookCtx, "recall: async job dead-lettered",
			otellog.String("job_id", string(rec.ID)),
			otellog.Int("attempts", rec.Attempts),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return
	}
	d := m.cfg.jobBackoffBase
	for i := 1; i < rec.Attempts; i++ {
		d *= 2
		if d >= m.cfg.jobBackoffMax {
			d = m.cfg.jobBackoffMax
			break
		}
	}
	if d > m.cfg.jobBackoffMax {
		d = m.cfg.jobBackoffMax
	}
	next := m.cfg.now().Add(d)
	_ = m.cfg.jobQueue.Reschedule(bookCtx, rec.ID, next, err.Error())
}
