package postgres

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/jackc/pgx/v5"
)

type sideEffectOutbox struct {
	b *Backend
}

func (q *sideEffectOutbox) Enqueue(ctx context.Context, job port.SideEffectJob) error {
	if job.Scope.PartitionKey() == "" || job.RequestID == "" {
		return nil
	}
	job = sqlstmt.CloneSideEffectJob(job)
	job.ID = sqlstmt.SideEffectJobID(job)
	raw, err := sqlstmt.EncodeJSON(job)
	if err != nil {
		return err
	}
	runtimeID, userID := sqlstmt.ScopeParts(job.Scope)
	_, err = q.b.pool.Exec(ctx,
		`INSERT INTO recall_side_effect_jobs(id, request_id, runtime_id, user_id, kind, status, enqueued_at_ns, retry_at_ns, lease_until_ns, lease_token, attempt, failure_class, failure_err, result_json, payload_json)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		 ON CONFLICT(id) DO NOTHING`,
		job.ID, job.RequestID, runtimeID, userID, string(job.Kind), sqlstmt.StatusPending, time.Now().UnixNano(), nil, nil, "", 0, "", "", "", raw)
	return err
}

func (q *sideEffectOutbox) Claim(ctx context.Context, opts port.SideEffectClaimOptions) ([]port.SideEffectJob, error) {
	if opts.Max <= 0 || opts.Scope.PartitionKey() == "" {
		return nil, nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	runtimeID, userID := sqlstmt.ScopeParts(opts.Scope)
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE recall_side_effect_jobs SET status = $1, lease_until_ns = NULL, lease_token = '' WHERE status = $2 AND lease_until_ns IS NOT NULL AND lease_until_ns <= $3`, sqlstmt.StatusPending, sqlstmt.StatusLeased, now.UnixNano()); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, payload_json FROM recall_side_effect_jobs
		 WHERE runtime_id = $1 AND user_id = $2 AND status = $3 AND (retry_at_ns IS NULL OR retry_at_ns <= $4)
		 ORDER BY enqueued_at_ns ASC, id ASC
		 LIMIT $5 FOR UPDATE SKIP LOCKED`,
		runtimeID, userID, sqlstmt.StatusPending, now.UnixNano(), opts.Max)
	if err != nil {
		return nil, err
	}
	type selectedRow struct{ id, raw string }
	selected := make([]selectedRow, 0)
	for rows.Next() {
		var r selectedRow
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			rows.Close()
			return nil, err
		}
		selected = append(selected, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]port.SideEffectJob, 0, len(selected))
	for _, r := range selected {
		job, err := sqlstmt.DecodeJSON[port.SideEffectJob](r.raw)
		if err != nil {
			return nil, err
		}
		job.Attempt++
		job.LeaseUntil = now.Add(sqlstmt.SideEffectLeaseTTL)
		job.LeaseToken = newLeaseToken()
		raw, err := sqlstmt.EncodeJSON(job)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE recall_side_effect_jobs SET status = $1, lease_until_ns = $2, lease_token = $3, attempt = $4, retry_at_ns = NULL, payload_json = $5 WHERE id = $6 AND status = $7`,
			sqlstmt.StatusLeased, job.LeaseUntil.UnixNano(), job.LeaseToken, job.Attempt, raw, r.id, sqlstmt.StatusPending); err != nil {
			return nil, err
		}
		out = append(out, sqlstmt.CloneSideEffectJob(job))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	_ = opts.WorkerID
	return out, nil
}

func (q *sideEffectOutbox) Complete(ctx context.Context, jobID, leaseToken string, result port.SideEffectResult) error {
	if jobID == "" {
		return nil
	}
	return q.mutateLeased(ctx, jobID, leaseToken, func(job *port.SideEffectJob) (string, port.SideEffectFailure, string, error) {
		job.Facts = sqlstmt.ScrubFacts(job.Facts)
		job.LeaseUntil = time.Time{}
		job.LeaseToken = ""
		rawResult, err := sqlstmt.EncodeJSON(result)
		return sqlstmt.StatusComplete, port.SideEffectFailure{}, rawResult, err
	})
}

func (q *sideEffectOutbox) Fail(ctx context.Context, jobID, leaseToken string, failure port.SideEffectFailure) error {
	if jobID == "" {
		return nil
	}
	return q.mutateLeased(ctx, jobID, leaseToken, func(job *port.SideEffectJob) (string, port.SideEffectFailure, string, error) {
		job.LeaseUntil = time.Time{}
		job.LeaseToken = ""
		if failure.ErrClass == diagnostic.ErrClassPermanent {
			job.Facts = sqlstmt.ScrubFacts(job.Facts)
			return sqlstmt.StatusFailed, failure, "", nil
		}
		return sqlstmt.StatusPending, failure, "", nil
	})
}

func (q *sideEffectOutbox) Cancel(ctx context.Context, requestID string) error {
	if requestID == "" {
		return nil
	}
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id, runtime_id, user_id FROM recall_side_effect_jobs WHERE request_id = $1 AND status <> $2`, requestID, sqlstmt.StatusComplete)
	if err != nil {
		return err
	}
	type row struct{ id, runtimeID, userID string }
	toDelete := make([]row, 0)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.runtimeID, &r.userID); err != nil {
			rows.Close()
			return err
		}
		toDelete = append(toDelete, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range toDelete {
		if _, err := tx.Exec(ctx, `DELETE FROM recall_side_effect_jobs WHERE id = $1`, r.id); err != nil {
			return err
		}
		if err := incrementCounter(ctx, tx, sqlstmt.CounterSideEffect, sqlstmt.ScopeFromParts(r.runtimeID, r.userID), 1); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (q *sideEffectOutbox) CancelScope(ctx context.Context, scope domain.Scope) (int, error) {
	if scope.PartitionKey() == "" {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id FROM recall_side_effect_jobs WHERE runtime_id = $1 AND user_id = $2 AND status <> $3`, runtimeID, userID, sqlstmt.StatusComplete)
	if err != nil {
		return 0, err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `DELETE FROM recall_side_effect_jobs WHERE id = $1`, id); err != nil {
			return 0, err
		}
	}
	if err := incrementCounter(ctx, tx, sqlstmt.CounterSideEffect, scope, len(ids)); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (q *sideEffectOutbox) PurgeScope(ctx context.Context, scope domain.Scope) (int, error) {
	if scope.PartitionKey() == "" {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM recall_side_effect_jobs WHERE runtime_id = $1 AND user_id = $2`, runtimeID, userID).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM recall_side_effect_jobs WHERE runtime_id = $1 AND user_id = $2`, runtimeID, userID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

func (q *sideEffectOutbox) Stats(ctx context.Context, scope domain.Scope, now time.Time) (port.SideEffectStats, error) {
	if scope.PartitionKey() == "" {
		return port.SideEffectStats{}, errdefs.Validationf("sideeffect: scope partition is required (RuntimeID and UserID)")
	}
	if now.IsZero() {
		now = time.Now()
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := q.b.pool.Query(ctx, `SELECT status, lease_until_ns, failure_class FROM recall_side_effect_jobs WHERE runtime_id = $1 AND user_id = $2`, runtimeID, userID)
	if err != nil {
		return port.SideEffectStats{}, err
	}
	defer rows.Close()
	var out port.SideEffectStats
	for rows.Next() {
		var status, failureClass string
		var leaseUntil *int64
		if err := rows.Scan(&status, &leaseUntil, &failureClass); err != nil {
			return port.SideEffectStats{}, err
		}
		switch status {
		case sqlstmt.StatusPending:
			out.Pending++
		case sqlstmt.StatusLeased:
			out.Leased++
			if leaseUntil != nil && *leaseUntil <= now.UnixNano() {
				out.ExpiredLeases++
			}
		case sqlstmt.StatusFailed:
			out.Failed++
			if diagnostic.ErrClass(failureClass) == diagnostic.ErrClassPermanent {
				out.DeadLetter++
			}
		case sqlstmt.StatusComplete:
			out.Completed++
		}
	}
	if err := rows.Err(); err != nil {
		return port.SideEffectStats{}, err
	}
	out.CancelledTotal, err = counter(ctx, q.b.pool, sqlstmt.CounterSideEffect, scope)
	return out, err
}

func (q *sideEffectOutbox) mutateLeased(ctx context.Context, jobID, leaseToken string, mutate func(*port.SideEffectJob) (string, port.SideEffectFailure, string, error)) error {
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status, currentToken, raw string
	err = tx.QueryRow(ctx, `SELECT status, lease_token, payload_json FROM recall_side_effect_jobs WHERE id = $1`, jobID).Scan(&status, &currentToken, &raw)
	if err == pgx.ErrNoRows || status == sqlstmt.StatusComplete {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if status != sqlstmt.StatusLeased || leaseToken == "" || currentToken != leaseToken {
		return tx.Commit(ctx)
	}
	job, err := sqlstmt.DecodeJSON[port.SideEffectJob](raw)
	if err != nil {
		return err
	}
	nextStatus, failure, resultRaw, err := mutate(&job)
	if err != nil {
		return err
	}
	var retryAt any
	if nextStatus == sqlstmt.StatusPending && !failure.RetryAt.IsZero() {
		retryAt = failure.RetryAt.UnixNano()
	}
	raw, err = sqlstmt.EncodeJSON(job)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE recall_side_effect_jobs SET status = $1, retry_at_ns = $2, lease_until_ns = NULL, lease_token = '', failure_class = $3, failure_err = $4, result_json = $5, payload_json = $6 WHERE id = $7`,
		nextStatus, retryAt, string(failure.ErrClass), failure.Err, resultRaw, raw, jobID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ port.SideEffectOutbox = (*sideEffectOutbox)(nil)
