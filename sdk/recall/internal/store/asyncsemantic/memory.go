// Package asyncsemantic provides an in-memory AsyncSemanticQueue for
// tests and the SDK's default zero-config development experience.
//
// Production callers MUST replace with a durable backend (e.g.
// sdk/jobqueue or a vessel resource). The in-memory queue has no
// outbox drainer — Enqueue itself is the durable boundary because
// "durable" here means "process-local". Restarting the process loses
// all enqueued jobs.
package asyncsemantic

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Queue is the in-memory implementation. NOT goroutine-safe across
// processes; safe for concurrent use within one process.
type Queue struct {
	mu        sync.Mutex
	byRequest map[string]*entry
	pending   []*entry
	leased    map[string]*entry
}

const (
	statusPending  = "pending"
	statusLeased   = "leased"
	statusComplete = "complete"
	statusFailed   = "failed"

	// defaultLeaseTTL is the lease window assigned by Claim when the
	// enqueued job carries a zero LeaseUntil. Workers crashing without
	// completing become re-claimable after this window — caller
	// pre-populated LeaseUntil overrides this default.
	defaultLeaseTTL = 5 * time.Minute
)

type entry struct {
	job        port.AsyncSemanticJob
	enqueuedAt time.Time
	status     string
	leaseUntil time.Time
	result     port.AsyncSemanticResult
	failure    port.AsyncSemanticFailure
}

// New returns an empty in-memory queue ready for Enqueue / Claim.
func New() *Queue {
	return &Queue{
		byRequest: make(map[string]*entry),
		leased:    make(map[string]*entry),
	}
}

// Enqueue is idempotent on job.RequestID. Re-enqueue of an existing
// requestID returns the existing receipt without modifying state.
func (q *Queue) Enqueue(_ context.Context, job port.AsyncSemanticJob) (port.AsyncSemanticReceipt, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if existing, ok := q.byRequest[job.RequestID]; ok {
		return port.AsyncSemanticReceipt{
			RequestID:  existing.job.RequestID,
			EnqueuedAt: existing.enqueuedAt,
			QueueDepth: q.pendingDepthLocked(),
		}, nil
	}

	now := time.Now()
	e := &entry{
		job:        port.CloneAsyncSemanticJob(job),
		enqueuedAt: now,
		status:     statusPending,
	}
	q.byRequest[job.RequestID] = e
	q.pending = append(q.pending, e)
	return port.AsyncSemanticReceipt{
		RequestID:  job.RequestID,
		EnqueuedAt: now,
		QueueDepth: q.pendingDepthLocked(),
	}, nil
}

// jobMatchesClaimFilter reports whether e is eligible for Claim under
// opts. Scope wins over RuntimeID when both are set.
func jobMatchesClaimFilter(job port.AsyncSemanticJob, opts port.AsyncSemanticClaimOptions) bool {
	if opts.Scope != nil {
		return job.Scope.CanonicalKey() == opts.Scope.CanonicalKey()
	}
	if opts.RuntimeID != "" {
		return job.Scope.RuntimeID == opts.RuntimeID
	}
	return true
}

// Claim picks up to opts.Max pending jobs in (enqueuedAt asc, scope,
// requestID) order, marks them leased, and applies optional scope /
// runtime filters for multi-tenant workers.
//
// Entries currently leased whose LeaseUntil has expired (relative to
// opts.Now) are re-eligible for claim, supporting the worker crash /
// lease-loss scenario.
func (q *Queue) Claim(_ context.Context, opts port.AsyncSemanticClaimOptions) ([]port.AsyncSemanticJob, error) {
	if opts.Max <= 0 {
		return nil, nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, e := range q.leased {
		if e.status != statusLeased {
			continue
		}
		if !e.leaseUntil.IsZero() && !now.Before(e.leaseUntil) {
			e.status = statusPending
			delete(q.leased, e.job.RequestID)
			q.pending = append(q.pending, e)
		}
	}

	sort.SliceStable(q.pending, func(i, j int) bool {
		if q.pending[i].enqueuedAt.Equal(q.pending[j].enqueuedAt) {
			si := q.pending[i].job.Scope.CanonicalKey()
			sj := q.pending[j].job.Scope.CanonicalKey()
			if si == sj {
				return q.pending[i].job.RequestID < q.pending[j].job.RequestID
			}
			return si < sj
		}
		return q.pending[i].enqueuedAt.Before(q.pending[j].enqueuedAt)
	})

	out := make([]port.AsyncSemanticJob, 0, opts.Max)
	remaining := q.pending[:0]
	for _, e := range q.pending {
		if len(out) >= opts.Max {
			remaining = append(remaining, e)
			continue
		}
		if !jobMatchesClaimFilter(e.job, opts) {
			remaining = append(remaining, e)
			continue
		}
		e.status = statusLeased
		if e.job.LeaseUntil.IsZero() {
			e.leaseUntil = now.Add(defaultLeaseTTL)
		} else {
			e.leaseUntil = e.job.LeaseUntil
		}
		q.leased[e.job.RequestID] = e
		job := e.job
		job.Attempt++
		job.LeaseUntil = e.leaseUntil
		_ = opts.WorkerID
		out = append(out, job)
	}
	q.pending = remaining
	return out, nil
}

// CancelMatchingEpisodes removes non-complete jobs in scope whose
// EpisodeFactIDs intersect deletedEpisodeFactIDs.
func (q *Queue) CancelMatchingEpisodes(_ context.Context, scope domain.Scope, deletedEpisodeFactIDs []string) (int, error) {
	if len(deletedEpisodeFactIDs) == 0 {
		return 0, nil
	}
	targets := make(map[string]struct{}, len(deletedEpisodeFactIDs))
	for _, id := range deletedEpisodeFactIDs {
		if id != "" {
			targets[id] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	key := scope.CanonicalKey()
	if key == "" {
		return 0, nil
	}
	var toCancel []string
	for requestID, e := range q.byRequest {
		if e.status == statusComplete {
			continue
		}
		if e.job.Scope.CanonicalKey() != key {
			continue
		}
		if jobReferencesDeletedEpisodes(e.job, targets) {
			toCancel = append(toCancel, requestID)
		}
	}
	n := 0
	for _, requestID := range toCancel {
		if err := q.cancelLocked(requestID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func jobReferencesDeletedEpisodes(job port.AsyncSemanticJob, targets map[string]struct{}) bool {
	for _, id := range job.EpisodeFactIDs {
		if _, ok := targets[id]; ok {
			return true
		}
	}
	return false
}

// cancelLocked removes one job. Caller must hold q.mu.
func (q *Queue) cancelLocked(requestID string) error {
	e, ok := q.byRequest[requestID]
	if !ok || e.status == statusComplete {
		return nil
	}
	delete(q.byRequest, requestID)
	delete(q.leased, requestID)
	remaining := q.pending[:0]
	for _, pe := range q.pending {
		if pe.job.RequestID != requestID {
			remaining = append(remaining, pe)
		}
	}
	q.pending = remaining
	return nil
}

// CancelScope removes every non-complete job in the scope partition.
func (q *Queue) CancelScope(_ context.Context, scope domain.Scope) (int, error) {
	key := scope.CanonicalKey()
	if key == "" {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for requestID, e := range q.byRequest {
		if e.status == statusComplete {
			continue
		}
		if e.job.Scope.CanonicalKey() != key {
			continue
		}
		if err := q.cancelLocked(requestID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Complete marks the job complete. Idempotent: re-completing an
// already-complete job is a no-op so workers can safely retry.
func (q *Queue) Complete(_ context.Context, requestID string, result port.AsyncSemanticResult) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byRequest[requestID]
	if !ok {
		return nil
	}
	if e.status == statusComplete {
		return nil
	}
	e.status = statusComplete
	e.result = result
	delete(q.leased, requestID)
	return nil
}

// Fail marks the job failed and records the failure metadata. Like
// Complete, Fail is idempotent — repeated calls update the recorded
// failure but never resurrect a completed job.
func (q *Queue) Fail(_ context.Context, requestID string, failure port.AsyncSemanticFailure) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byRequest[requestID]
	if !ok {
		return nil
	}
	if e.status == statusComplete {
		return nil
	}
	e.status = statusFailed
	e.failure = failure
	delete(q.leased, requestID)
	return nil
}

// Cancel removes a pending or leased job. Idempotent when the job is
// unknown or already complete.
func (q *Queue) Cancel(_ context.Context, requestID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cancelLocked(requestID)
}

func (q *Queue) pendingDepthLocked() int {
	n := len(q.pending)
	for _, e := range q.leased {
		if e.status == statusLeased {
			n++
		}
	}
	return n
}

var _ port.AsyncSemanticQueue = (*Queue)(nil)
