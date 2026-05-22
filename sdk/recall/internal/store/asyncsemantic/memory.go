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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Queue is the in-memory implementation. NOT goroutine-safe across
// processes; safe for concurrent use within one process.
type Queue struct {
	mu        sync.Mutex
	byRequest map[string]*entry
	pending   []*entry
	leased    map[string]*entry

	cancelledByPartition map[string]int
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

	// defaultTransientRetryBackoff applies when Fail omits RetryAt.
	defaultTransientRetryBackoff = 30 * time.Second
)

type entry struct {
	job        port.AsyncSemanticJob
	enqueuedAt time.Time
	status     string
	// leaseUntil is the worker lease while leased, or a retry/hold
	// instant while pending (transient Fail or enqueue-time hint).
	leaseUntil time.Time
	leaseToken string
	wasLeased  bool
	result     port.AsyncSemanticResult
	failure    port.AsyncSemanticFailure
}

// New returns an empty in-memory queue ready for Enqueue / Claim.
func New() *Queue {
	return &Queue{
		byRequest:            make(map[string]*entry),
		leased:               make(map[string]*entry),
		cancelledByPartition: make(map[string]int),
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
	if !job.LeaseUntil.IsZero() {
		e.leaseUntil = job.LeaseUntil
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
		return job.Scope.PartitionKey() == opts.Scope.PartitionKey()
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
			e.leaseUntil = time.Time{}
			e.leaseToken = ""
			delete(q.leased, e.job.RequestID)
			q.pending = append(q.pending, e)
		}
	}

	sort.SliceStable(q.pending, func(i, j int) bool {
		if q.pending[i].enqueuedAt.Equal(q.pending[j].enqueuedAt) {
			si := q.pending[i].job.Scope.PartitionKey()
			sj := q.pending[j].job.Scope.PartitionKey()
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
		if !e.leaseUntil.IsZero() && now.Before(e.leaseUntil) {
			remaining = append(remaining, e)
			continue
		}
		e.status = statusLeased
		var workerLease time.Time
		if !e.wasLeased && !e.leaseUntil.IsZero() && !e.leaseUntil.After(now) {
			// Enqueue-time expired lease fixture (TestQueue_LeaseExpiry).
			workerLease = e.leaseUntil
		} else {
			workerLease = now.Add(defaultLeaseTTL)
		}
		e.leaseUntil = workerLease
		e.leaseToken = newLeaseToken()
		e.wasLeased = true
		e.job.Attempt++
		e.job.LeaseUntil = workerLease
		e.job.LeaseToken = e.leaseToken
		q.leased[e.job.RequestID] = e
		job := port.CloneAsyncSemanticJob(e.job)
		job.LeaseToken = e.leaseToken
		_ = opts.WorkerID
		out = append(out, job)
	}
	q.pending = remaining
	return out, nil
}

func newLeaseToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b[:])
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
	key := scope.PartitionKey()
	if key == "" {
		return 0, nil
	}
	var toCancel []string
	for requestID, e := range q.byRequest {
		if e.status == statusComplete {
			continue
		}
		if e.job.Scope.PartitionKey() != key {
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
	if key := e.job.Scope.PartitionKey(); key != "" {
		q.cancelledByPartition[key]++
	}
	return nil
}

// Stats returns queue health counters for one scope partition.
func (q *Queue) Stats(_ context.Context, filter port.AsyncSemanticStatsFilter) (port.AsyncSemanticStats, error) {
	now := filter.Now
	if now.IsZero() {
		now = time.Now()
	}
	partitionKey := filter.Scope.PartitionKey()
	if partitionKey == "" {
		return port.AsyncSemanticStats{}, fmt.Errorf("asyncsemantic: scope partition is required")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	var out port.AsyncSemanticStats
	if partitionKey != "" {
		out.CancelledTotal = q.cancelledByPartition[partitionKey]
	}
	for _, e := range q.byRequest {
		if partitionKey != "" && e.job.Scope.PartitionKey() != partitionKey {
			continue
		}
		switch e.status {
		case statusPending:
			out.Pending++
		case statusLeased:
			out.Leased++
			if !e.leaseUntil.IsZero() && !now.Before(e.leaseUntil) {
				out.ExpiredLeases++
			}
		case statusFailed:
			out.Failed++
			if e.failure.ErrClass == diagnostic.ErrClassPermanent {
				out.DeadLetter++
			}
		case statusComplete:
			out.Completed++
		}
	}
	return out, nil
}

// CancelScope removes every non-complete job in the scope partition.
func (q *Queue) CancelScope(_ context.Context, scope domain.Scope) (int, error) {
	key := scope.PartitionKey()
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
		if e.job.Scope.PartitionKey() != key {
			continue
		}
		if err := q.cancelLocked(requestID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// PurgeScope deletes every job in the partition, including completed
// and dead-letter rows, so enqueue-time PII snapshots cannot survive
// ForgetAll(Hard).
func (q *Queue) PurgeScope(_ context.Context, scope domain.Scope) (int, error) {
	key := scope.PartitionKey()
	if key == "" {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var remove []string
	for requestID, e := range q.byRequest {
		if e.job.Scope.PartitionKey() != key {
			continue
		}
		remove = append(remove, requestID)
	}
	for _, requestID := range remove {
		delete(q.byRequest, requestID)
		delete(q.leased, requestID)
	}
	if len(remove) > 0 {
		filtered := q.pending[:0]
		for _, pe := range q.pending {
			if pe.job.Scope.PartitionKey() != key {
				filtered = append(filtered, pe)
			}
		}
		q.pending = filtered
	}
	return len(remove), nil
}

// leaseAckAccepted reports whether Complete/Fail may mutate a job.
// Only the current non-empty leased token is accepted; jobs that
// returned to pending after lease expiry are not ackable with a
// stale token.
func leaseAckAccepted(e *entry, leaseToken string) bool {
	if e == nil || e.status != statusLeased {
		return false
	}
	if leaseToken == "" || e.leaseToken == "" {
		return false
	}
	return leaseToken == e.leaseToken
}

// Complete marks the job complete. Idempotent: re-completing an
// already-complete job is a no-op so workers can safely retry.
// Stale or empty lease tokens are ignored.
func (q *Queue) Complete(_ context.Context, requestID, leaseToken string, result port.AsyncSemanticResult) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byRequest[requestID]
	if !ok {
		return nil
	}
	if e.status == statusComplete {
		return nil
	}
	if !leaseAckAccepted(e, leaseToken) {
		return nil
	}
	e.status = statusComplete
	e.result = result
	e.leaseToken = ""
	port.ScrubAsyncSemanticJobPII(&e.job)
	delete(q.leased, requestID)
	return nil
}

// Fail records failure metadata. Permanent failures become dead-letter
// (statusFailed). Transient failures re-enter pending with RetryAt /
// default backoff so Claim can pick them up after the retry window.
func (q *Queue) Fail(_ context.Context, requestID, leaseToken string, failure port.AsyncSemanticFailure) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byRequest[requestID]
	if !ok {
		return nil
	}
	if e.status == statusComplete || e.status == statusFailed {
		return nil
	}
	if !leaseAckAccepted(e, leaseToken) {
		return nil
	}
	delete(q.leased, requestID)
	e.leaseToken = ""
	if failure.ErrClass == diagnostic.ErrClassPermanent {
		e.status = statusFailed
		e.failure = failure
		port.ScrubAsyncSemanticJobPII(&e.job)
		return nil
	}
	now := time.Now()
	retryAt := failure.RetryAt
	if retryAt.IsZero() || !retryAt.After(now) {
		retryAt = now.Add(defaultTransientRetryBackoff)
	}
	e.job.Attempt++
	e.job.LeaseUntil = retryAt
	e.leaseUntil = retryAt
	e.failure = failure
	e.status = statusPending
	q.pending = append(q.pending, e)
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
