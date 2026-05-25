package workspace

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
)

type asyncSemanticQueue struct {
	b *Backend
}

func (q *asyncSemanticQueue) Enqueue(ctx context.Context, job port.AsyncSemanticJob) (port.AsyncSemanticReceipt, error) {
	if job.RequestID == "" {
		return port.AsyncSemanticReceipt{}, nil
	}
	job = port.CloneAsyncSemanticJob(job)
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return port.AsyncSemanticReceipt{}, err
	}
	if idx := asyncIndex(st.Async, job.RequestID); idx >= 0 {
		return port.AsyncSemanticReceipt{
			RequestID:  st.Async[idx].Job.RequestID,
			EnqueuedAt: st.Async[idx].EnqueuedAt,
			QueueDepth: asyncPendingDepth(st, job.Scope),
		}, nil
	}
	now := time.Now()
	st.Async = append(st.Async, asyncSemanticRecord{
		Job:        job,
		Status:     sqlstmt.StatusPending,
		EnqueuedAt: now,
	})
	depth := asyncPendingDepth(st, job.Scope)
	if err := q.b.save(ctx, st); err != nil {
		return port.AsyncSemanticReceipt{}, err
	}
	return port.AsyncSemanticReceipt{RequestID: job.RequestID, EnqueuedAt: now, QueueDepth: depth}, nil
}

func (q *asyncSemanticQueue) Cancel(ctx context.Context, requestID string) error {
	if requestID == "" {
		return nil
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return err
	}
	idx := asyncIndex(st.Async, requestID)
	if idx < 0 || st.Async[idx].Status == sqlstmt.StatusComplete {
		return nil
	}
	scope := st.Async[idx].Job.Scope
	st.Async = append(st.Async[:idx], st.Async[idx+1:]...)
	incrementAsyncCancelled(&st, scope, 1)
	return q.b.save(ctx, st)
}

func (q *asyncSemanticQueue) CancelScope(ctx context.Context, scope domain.Scope) (int, error) {
	if scope.PartitionKey() == "" {
		return 0, nil
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	filtered := st.Async[:0]
	for _, r := range st.Async {
		if samePartition(r.Job.Scope, scope) && r.Status != sqlstmt.StatusComplete {
			n++
			continue
		}
		filtered = append(filtered, r)
	}
	st.Async = filtered
	incrementAsyncCancelled(&st, scope, n)
	if err := q.b.save(ctx, st); err != nil {
		return 0, err
	}
	return n, nil
}

func (q *asyncSemanticQueue) PurgeScope(ctx context.Context, scope domain.Scope) (int, error) {
	if scope.PartitionKey() == "" {
		return 0, nil
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	filtered := st.Async[:0]
	for _, r := range st.Async {
		if samePartition(r.Job.Scope, scope) {
			n++
			continue
		}
		filtered = append(filtered, r)
	}
	st.Async = filtered
	if err := q.b.save(ctx, st); err != nil {
		return 0, err
	}
	return n, nil
}

func (q *asyncSemanticQueue) CancelMatchingEpisodes(ctx context.Context, scope domain.Scope, deletedEpisodeFactIDs []string) (int, error) {
	if scope.PartitionKey() == "" || len(deletedEpisodeFactIDs) == 0 {
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
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	filtered := st.Async[:0]
	for _, r := range st.Async {
		if samePartition(r.Job.Scope, scope) && r.Status != sqlstmt.StatusComplete && asyncReferencesEpisode(r.Job, targets) {
			n++
			continue
		}
		filtered = append(filtered, r)
	}
	st.Async = filtered
	incrementAsyncCancelled(&st, scope, n)
	if err := q.b.save(ctx, st); err != nil {
		return 0, err
	}
	return n, nil
}

func (q *asyncSemanticQueue) Claim(ctx context.Context, opts port.AsyncSemanticClaimOptions) ([]port.AsyncSemanticJob, error) {
	if opts.Max <= 0 {
		return nil, nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return nil, err
	}
	for i := range st.Async {
		r := &st.Async[i]
		if r.Status == sqlstmt.StatusLeased && !r.Job.LeaseUntil.IsZero() && !now.Before(r.Job.LeaseUntil) {
			r.Status = sqlstmt.StatusPending
			r.Job.LeaseUntil = time.Time{}
			r.Job.LeaseToken = ""
		}
	}
	out := make([]port.AsyncSemanticJob, 0, opts.Max)
	for i := range st.Async {
		if len(out) >= opts.Max {
			break
		}
		r := &st.Async[i]
		if r.Status != sqlstmt.StatusPending || !asyncMatchesClaim(r.Job, opts) {
			continue
		}
		if !r.Job.LeaseUntil.IsZero() && now.Before(r.Job.LeaseUntil) {
			continue
		}
		r.Status = sqlstmt.StatusLeased
		r.Job.Attempt++
		r.Job.LeaseUntil = now.Add(sqlstmt.AsyncSemanticLeaseTTL)
		r.Job.LeaseToken = newLeaseToken()
		out = append(out, port.CloneAsyncSemanticJob(r.Job))
	}
	if err := q.b.save(ctx, st); err != nil {
		return nil, err
	}
	_ = opts.WorkerID
	return out, nil
}

func (q *asyncSemanticQueue) Complete(ctx context.Context, requestID, leaseToken string, result port.AsyncSemanticResult) error {
	return q.mutateLeased(ctx, requestID, leaseToken, func(r *asyncSemanticRecord) {
		r.Status = sqlstmt.StatusComplete
		r.Job.LeaseUntil = time.Time{}
		r.Job.LeaseToken = ""
		r.Result = result
		port.ScrubAsyncSemanticJobPII(&r.Job)
	})
}

func (q *asyncSemanticQueue) Fail(ctx context.Context, requestID, leaseToken string, failure port.AsyncSemanticFailure) error {
	return q.mutateLeased(ctx, requestID, leaseToken, func(r *asyncSemanticRecord) {
		r.Job.LeaseToken = ""
		r.Failure = failure
		if failure.ErrClass == diagnostic.ErrClassPermanent {
			r.Status = sqlstmt.StatusFailed
			r.Job.LeaseUntil = time.Time{}
			port.ScrubAsyncSemanticJobPII(&r.Job)
			return
		}
		r.Status = sqlstmt.StatusPending
		retryAt := failure.RetryAt
		if retryAt.IsZero() || !retryAt.After(time.Now()) {
			retryAt = time.Now().Add(sqlstmt.AsyncRetryBackoff)
		}
		r.Job.LeaseUntil = retryAt
	})
}

func (q *asyncSemanticQueue) Stats(ctx context.Context, filter port.AsyncSemanticStatsFilter) (port.AsyncSemanticStats, error) {
	if err := ensurePartition(filter.Scope, "asyncsemantic"); err != nil {
		return port.AsyncSemanticStats{}, err
	}
	now := filter.Now
	if now.IsZero() {
		now = time.Now()
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return port.AsyncSemanticStats{}, err
	}
	var out port.AsyncSemanticStats
	out.CancelledTotal = partitionCounter(&st, filter.Scope).AsyncSemantic
	for _, r := range st.Async {
		if !samePartition(r.Job.Scope, filter.Scope) {
			continue
		}
		switch r.Status {
		case sqlstmt.StatusPending:
			out.Pending++
		case sqlstmt.StatusLeased:
			out.Leased++
			if !r.Job.LeaseUntil.IsZero() && !now.Before(r.Job.LeaseUntil) {
				out.ExpiredLeases++
			}
		case sqlstmt.StatusFailed:
			out.Failed++
			if r.Failure.ErrClass == diagnostic.ErrClassPermanent {
				out.DeadLetter++
			}
		case sqlstmt.StatusComplete:
			out.Completed++
		}
	}
	return out, nil
}

func (q *asyncSemanticQueue) mutateLeased(ctx context.Context, requestID, leaseToken string, mutate func(*asyncSemanticRecord)) error {
	if requestID == "" {
		return nil
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return err
	}
	idx := asyncIndex(st.Async, requestID)
	if idx < 0 {
		return nil
	}
	r := &st.Async[idx]
	if r.Status == sqlstmt.StatusComplete || r.Status == sqlstmt.StatusFailed {
		return nil
	}
	if r.Status != sqlstmt.StatusLeased || leaseToken == "" || r.Job.LeaseToken != leaseToken {
		return nil
	}
	mutate(r)
	return q.b.save(ctx, st)
}

func asyncIndex(records []asyncSemanticRecord, requestID string) int {
	for i, r := range records {
		if r.Job.RequestID == requestID {
			return i
		}
	}
	return -1
}

func asyncPendingDepth(st state, scope domain.Scope) int {
	n := 0
	for _, r := range st.Async {
		if samePartition(r.Job.Scope, scope) && r.Status == sqlstmt.StatusPending {
			n++
		}
	}
	return n
}

func asyncMatchesClaim(job port.AsyncSemanticJob, opts port.AsyncSemanticClaimOptions) bool {
	if opts.Scope != nil {
		return job.Scope.PartitionKey() == opts.Scope.PartitionKey()
	}
	if opts.RuntimeID != "" {
		return job.Scope.RuntimeID == opts.RuntimeID
	}
	return true
}

func asyncReferencesEpisode(job port.AsyncSemanticJob, targets map[string]struct{}) bool {
	for _, id := range job.EpisodeFactIDs {
		if _, ok := targets[id]; ok {
			return true
		}
	}
	return false
}
