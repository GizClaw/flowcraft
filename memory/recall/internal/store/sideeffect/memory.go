// Package sideeffect provides the in-memory SideEffectOutbox used by
// tests and the SDK default stack. Enqueue is the durable boundary
// inside the scope write lock; processors claim and execute jobs
// outside the lock.
package sideeffect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Queue is an in-memory SideEffectOutbox. Jobs are lost on restart.
type Queue struct {
	mu                   sync.Mutex
	pending              map[string][]*entry // partition key -> FIFO
	byID                 map[string]*entry
	leased               map[string]*entry
	cancelledByPartition map[string]int
}

type entry struct {
	job        port.SideEffectJob
	status     status
	enqueuedAt time.Time
	retryAt    time.Time
	failure    port.SideEffectFailure
}

type status string

const (
	statusPending   status = "pending"
	statusLeased    status = "leased"
	statusFailed    status = "failed"
	statusComplete  status = "complete"
	defaultLeaseTTL        = 30 * time.Second
)

// New returns an empty side-effect outbox.
func New() *Queue {
	return &Queue{
		pending:              make(map[string][]*entry),
		byID:                 make(map[string]*entry),
		leased:               make(map[string]*entry),
		cancelledByPartition: make(map[string]int),
	}
}

func partitionKey(scope domain.Scope) string {
	return scope.PartitionKey()
}

// Enqueue appends a job for the scope partition.
func (q *Queue) Enqueue(_ context.Context, job port.SideEffectJob) error {
	key := partitionKey(job.Scope)
	if key == "" || job.RequestID == "" {
		return nil
	}
	if job.ID == "" {
		job.ID = fmt.Sprintf("%s|%s", job.RequestID, job.Kind)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.byID[job.ID]; exists {
		return nil
	}
	e := &entry{job: cloneJob(job), status: statusPending, enqueuedAt: time.Now()}
	q.pending[key] = append(q.pending[key], e)
	q.byID[job.ID] = e
	return nil
}

// Claim leases pending jobs for a scope partition.
func (q *Queue) Claim(_ context.Context, opts port.SideEffectClaimOptions) ([]port.SideEffectJob, error) {
	if opts.Max <= 0 {
		return nil, nil
	}
	key := partitionKey(opts.Scope)
	if key == "" {
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
		if !e.job.LeaseUntil.IsZero() && !now.Before(e.job.LeaseUntil) {
			e.status = statusPending
			e.job.LeaseUntil = time.Time{}
			e.job.LeaseToken = ""
			delete(q.leased, e.job.ID)
			leaseKey := partitionKey(e.job.Scope)
			q.pending[leaseKey] = append(q.pending[leaseKey], e)
		}
	}

	out := make([]port.SideEffectJob, 0, opts.Max)
	remaining := q.pending[key][:0]
	for i, e := range q.pending[key] {
		if len(out) >= opts.Max {
			remaining = append(remaining, e)
			continue
		}
		if !e.retryAt.IsZero() && now.Before(e.retryAt) {
			remaining = append(remaining, e)
			remaining = append(remaining, q.pending[key][i+1:]...)
			break
		}
		e.status = statusLeased
		e.job.Attempt++
		e.job.LeaseUntil = now.Add(defaultLeaseTTL)
		e.job.LeaseToken = newLeaseToken()
		q.leased[e.job.ID] = e
		out = append(out, cloneJob(e.job))
	}
	if len(remaining) == 0 {
		delete(q.pending, key)
	} else {
		q.pending[key] = remaining
	}
	return out, nil
}

// Complete marks the current leased job complete. Stale tokens are ignored.
func (q *Queue) Complete(_ context.Context, jobID, leaseToken string, result port.SideEffectResult) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.byID[jobID]
	if e == nil || e.status == statusComplete {
		return nil
	}
	if !leaseAckAccepted(e, leaseToken) {
		return nil
	}
	e.status = statusComplete
	e.job.Facts = scrubFacts(e.job.Facts)
	e.job.LeaseToken = ""
	e.job.LeaseUntil = time.Time{}
	delete(q.leased, jobID)
	_ = result
	return nil
}

// Fail records a worker failure. Permanent failures dead-letter; all
// others return to pending after RetryAt.
func (q *Queue) Fail(_ context.Context, jobID, leaseToken string, failure port.SideEffectFailure) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.byID[jobID]
	if e == nil || e.status == statusComplete {
		return nil
	}
	if !leaseAckAccepted(e, leaseToken) {
		return nil
	}
	e.failure = failure
	e.job.LeaseToken = ""
	e.job.LeaseUntil = time.Time{}
	delete(q.leased, jobID)
	if failure.ErrClass == diagnostic.ErrClassPermanent {
		e.status = statusFailed
		e.job.Facts = scrubFacts(e.job.Facts)
		return nil
	}
	e.status = statusPending
	e.retryAt = failure.RetryAt
	key := partitionKey(e.job.Scope)
	q.pending[key] = append(q.pending[key], e)
	return nil
}

func leaseAckAccepted(e *entry, leaseToken string) bool {
	return e != nil && e.status == statusLeased && leaseToken != "" && e.job.LeaseToken == leaseToken
}

func newLeaseToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b[:])
}

// Cancel removes every non-complete job with the given Save batch RequestID.
func (q *Queue) Cancel(_ context.Context, requestID string) error {
	if requestID == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, e := range q.byID {
		if e.job.RequestID == requestID && e.status != statusComplete {
			q.cancelLocked(id)
		}
	}
	return nil
}

// CancelScope drops all non-complete jobs in the partition.
func (q *Queue) CancelScope(_ context.Context, scope domain.Scope) (int, error) {
	key := partitionKey(scope)
	if key == "" {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for id, e := range q.byID {
		if e.status == statusComplete || partitionKey(e.job.Scope) != key {
			continue
		}
		q.cancelLocked(id)
		n++
	}
	return n, nil
}

// PurgeScope deletes every job in the partition, including terminal rows.
func (q *Queue) PurgeScope(_ context.Context, scope domain.Scope) (int, error) {
	key := partitionKey(scope)
	if key == "" {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for id, e := range q.byID {
		if partitionKey(e.job.Scope) != key {
			continue
		}
		delete(q.byID, id)
		delete(q.leased, id)
		n++
	}
	delete(q.pending, key)
	return n, nil
}

// Stats returns queue health counters for a partition.
func (q *Queue) Stats(_ context.Context, scope domain.Scope, now time.Time) (port.SideEffectStats, error) {
	key := partitionKey(scope)
	if key == "" {
		return port.SideEffectStats{}, fmt.Errorf("sideeffect: scope partition is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out port.SideEffectStats
	out.CancelledTotal = q.cancelledByPartition[key]
	for _, e := range q.byID {
		if key != "" && partitionKey(e.job.Scope) != key {
			continue
		}
		switch e.status {
		case statusPending:
			out.Pending++
		case statusLeased:
			out.Leased++
			if !e.job.LeaseUntil.IsZero() && !now.Before(e.job.LeaseUntil) {
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

func scrubFacts(facts []domain.TemporalFact) []domain.TemporalFact {
	if len(facts) == 0 {
		return nil
	}
	out := make([]domain.TemporalFact, 0, len(facts))
	for _, f := range facts {
		out = append(out, domain.TemporalFact{
			ID:    f.ID,
			Scope: f.Scope,
			Kind:  f.Kind,
		})
	}
	return out
}

func (q *Queue) cancelLocked(jobID string) {
	e := q.byID[jobID]
	if e == nil || e.status == statusComplete {
		return
	}
	key := partitionKey(e.job.Scope)
	delete(q.byID, jobID)
	delete(q.leased, jobID)
	filtered := q.pending[key][:0]
	for _, pe := range q.pending[key] {
		if pe.job.ID != jobID {
			filtered = append(filtered, pe)
		}
	}
	if len(filtered) == 0 {
		delete(q.pending, key)
	} else {
		q.pending[key] = filtered
	}
	q.cancelledByPartition[key]++
}

func cloneJob(job port.SideEffectJob) port.SideEffectJob {
	out := job
	if len(job.Facts) > 0 {
		out.Facts = make([]domain.TemporalFact, len(job.Facts))
		for i, f := range job.Facts {
			out.Facts[i] = f.Clone()
		}
	}
	return out
}

var _ port.SideEffectOutbox = (*Queue)(nil)
