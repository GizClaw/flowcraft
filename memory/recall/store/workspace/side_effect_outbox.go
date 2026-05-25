package workspace

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
)

type sideEffectOutbox struct {
	b *Backend
}

func (q *sideEffectOutbox) Enqueue(ctx context.Context, job port.SideEffectJob) error {
	if job.Scope.PartitionKey() == "" || job.RequestID == "" {
		return nil
	}
	job = cloneSideEffectJob(job)
	job.ID = sqlstmt.SideEffectJobID(job)
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return err
	}
	if sideEffectIndex(st.SideEffects, job.ID) >= 0 {
		return nil
	}
	st.SideEffects = append(st.SideEffects, sideEffectRecord{
		Job:        job,
		Status:     sqlstmt.StatusPending,
		EnqueuedAt: time.Now(),
	})
	return q.b.save(ctx, st)
}

func (q *sideEffectOutbox) Claim(ctx context.Context, opts port.SideEffectClaimOptions) ([]port.SideEffectJob, error) {
	if opts.Max <= 0 || opts.Scope.PartitionKey() == "" {
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
	for i := range st.SideEffects {
		r := &st.SideEffects[i]
		if r.Status == sqlstmt.StatusLeased && !r.Job.LeaseUntil.IsZero() && !now.Before(r.Job.LeaseUntil) {
			r.Status = sqlstmt.StatusPending
			r.Job.LeaseUntil = time.Time{}
			r.Job.LeaseToken = ""
		}
	}
	out := make([]port.SideEffectJob, 0, opts.Max)
	for i := range st.SideEffects {
		if len(out) >= opts.Max {
			break
		}
		r := &st.SideEffects[i]
		if r.Status != sqlstmt.StatusPending || !samePartition(r.Job.Scope, opts.Scope) {
			continue
		}
		if !r.RetryAt.IsZero() && now.Before(r.RetryAt) {
			continue
		}
		r.Status = sqlstmt.StatusLeased
		r.Job.Attempt++
		r.Job.LeaseUntil = now.Add(sqlstmt.SideEffectLeaseTTL)
		r.Job.LeaseToken = newLeaseToken()
		r.RetryAt = time.Time{}
		out = append(out, cloneSideEffectJob(r.Job))
	}
	if err := q.b.save(ctx, st); err != nil {
		return nil, err
	}
	_ = opts.WorkerID
	return out, nil
}

func (q *sideEffectOutbox) Complete(ctx context.Context, jobID, leaseToken string, result port.SideEffectResult) error {
	return q.mutateLeased(ctx, jobID, leaseToken, func(r *sideEffectRecord) {
		r.Status = sqlstmt.StatusComplete
		r.Job.Facts = sqlstmt.ScrubFacts(r.Job.Facts)
		r.Job.LeaseUntil = time.Time{}
		r.Job.LeaseToken = ""
		r.Result = result
	})
}

func (q *sideEffectOutbox) Fail(ctx context.Context, jobID, leaseToken string, failure port.SideEffectFailure) error {
	return q.mutateLeased(ctx, jobID, leaseToken, func(r *sideEffectRecord) {
		r.Job.LeaseUntil = time.Time{}
		r.Job.LeaseToken = ""
		r.Failure = failure
		if failure.ErrClass == diagnostic.ErrClassPermanent {
			r.Status = sqlstmt.StatusFailed
			r.Job.Facts = sqlstmt.ScrubFacts(r.Job.Facts)
			return
		}
		r.Status = sqlstmt.StatusPending
		r.RetryAt = failure.RetryAt
	})
}

func (q *sideEffectOutbox) Cancel(ctx context.Context, requestID string) error {
	if requestID == "" {
		return nil
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return err
	}
	filtered := st.SideEffects[:0]
	for _, r := range st.SideEffects {
		if r.Job.RequestID == requestID && r.Status != sqlstmt.StatusComplete {
			incrementSideEffectCancelled(&st, r.Job.Scope, 1)
			continue
		}
		filtered = append(filtered, r)
	}
	st.SideEffects = filtered
	return q.b.save(ctx, st)
}

func (q *sideEffectOutbox) CancelScope(ctx context.Context, scope domain.Scope) (int, error) {
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
	filtered := st.SideEffects[:0]
	for _, r := range st.SideEffects {
		if samePartition(r.Job.Scope, scope) && r.Status != sqlstmt.StatusComplete {
			n++
			continue
		}
		filtered = append(filtered, r)
	}
	st.SideEffects = filtered
	incrementSideEffectCancelled(&st, scope, n)
	if err := q.b.save(ctx, st); err != nil {
		return 0, err
	}
	return n, nil
}

func (q *sideEffectOutbox) PurgeScope(ctx context.Context, scope domain.Scope) (int, error) {
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
	filtered := st.SideEffects[:0]
	for _, r := range st.SideEffects {
		if samePartition(r.Job.Scope, scope) {
			n++
			continue
		}
		filtered = append(filtered, r)
	}
	st.SideEffects = filtered
	if err := q.b.save(ctx, st); err != nil {
		return 0, err
	}
	return n, nil
}

func (q *sideEffectOutbox) Stats(ctx context.Context, scope domain.Scope, now time.Time) (port.SideEffectStats, error) {
	if err := ensurePartition(scope, "sideeffect"); err != nil {
		return port.SideEffectStats{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return port.SideEffectStats{}, err
	}
	var out port.SideEffectStats
	out.CancelledTotal = partitionCounter(&st, scope).SideEffect
	for _, r := range st.SideEffects {
		if !samePartition(r.Job.Scope, scope) {
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

func (q *sideEffectOutbox) mutateLeased(ctx context.Context, jobID, leaseToken string, mutate func(*sideEffectRecord)) error {
	if jobID == "" {
		return nil
	}
	q.b.mu.Lock()
	defer q.b.mu.Unlock()
	st, err := q.b.load(ctx)
	if err != nil {
		return err
	}
	idx := sideEffectIndex(st.SideEffects, jobID)
	if idx < 0 {
		return nil
	}
	r := &st.SideEffects[idx]
	if r.Status == sqlstmt.StatusComplete {
		return nil
	}
	if r.Status != sqlstmt.StatusLeased || leaseToken == "" || r.Job.LeaseToken != leaseToken {
		return nil
	}
	mutate(r)
	return q.b.save(ctx, st)
}

func sideEffectIndex(records []sideEffectRecord, jobID string) int {
	for i, r := range records {
		if r.Job.ID == jobID {
			return i
		}
	}
	return -1
}
