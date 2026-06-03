package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/jackc/pgx/v5"
)

type asyncSemanticQueue struct {
	b *Backend
}

func (q *asyncSemanticQueue) Enqueue(ctx context.Context, job port.AsyncSemanticJob) (port.AsyncSemanticReceipt, error) {
	if job.RequestID == "" {
		return port.AsyncSemanticReceipt{}, nil
	}
	job = port.CloneAsyncSemanticJob(job)
	runtimeID, userID := sqlstmt.ScopeParts(job.Scope)
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return port.AsyncSemanticReceipt{}, err
	}
	defer tx.Rollback(ctx)
	var enqueuedAt int64
	err = tx.QueryRow(ctx, `SELECT enqueued_at_ns FROM recall_async_semantic_jobs WHERE request_id = $1`, job.RequestID).Scan(&enqueuedAt)
	if err == nil {
		depth, err := q.pendingDepthTx(ctx, tx, job.Scope)
		if err != nil {
			return port.AsyncSemanticReceipt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return port.AsyncSemanticReceipt{}, err
		}
		return port.AsyncSemanticReceipt{RequestID: job.RequestID, EnqueuedAt: time.Unix(0, enqueuedAt).UTC(), QueueDepth: depth}, nil
	}
	if err != pgx.ErrNoRows {
		return port.AsyncSemanticReceipt{}, err
	}
	now := time.Now()
	raw, err := sqlstmt.EncodeJSON(job)
	if err != nil {
		return port.AsyncSemanticReceipt{}, err
	}
	var leaseUntil any
	if !job.LeaseUntil.IsZero() {
		leaseUntil = job.LeaseUntil.UnixNano()
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO recall_async_semantic_jobs(request_id, runtime_id, user_id, status, enqueued_at_ns, lease_until_ns, lease_token, attempt, failure_class, failure_err, result_json, payload_json)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		job.RequestID, runtimeID, userID, sqlstmt.StatusPending, now.UnixNano(), leaseUntil, "", 0, "", "", "", raw); err != nil {
		return port.AsyncSemanticReceipt{}, err
	}
	if err := q.replaceEpisodeRowsTx(ctx, tx, job); err != nil {
		return port.AsyncSemanticReceipt{}, err
	}
	depth, err := q.pendingDepthTx(ctx, tx, job.Scope)
	if err != nil {
		return port.AsyncSemanticReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return port.AsyncSemanticReceipt{}, err
	}
	return port.AsyncSemanticReceipt{RequestID: job.RequestID, EnqueuedAt: now, QueueDepth: depth}, nil
}

func (q *asyncSemanticQueue) Requeue(ctx context.Context, job port.AsyncSemanticJob) (port.AsyncSemanticReceipt, bool, error) {
	if job.RequestID == "" {
		return port.AsyncSemanticReceipt{}, false, nil
	}
	job = port.CloneAsyncSemanticJob(job)
	job.Attempt = 0
	job.LeaseUntil = time.Time{}
	job.LeaseToken = ""
	runtimeID, userID := sqlstmt.ScopeParts(job.Scope)
	raw, err := sqlstmt.EncodeJSON(job)
	if err != nil {
		return port.AsyncSemanticReceipt{}, false, err
	}
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return port.AsyncSemanticReceipt{}, false, err
	}
	defer tx.Rollback(ctx)
	now := time.Now()
	enqueuedAt := now.UnixNano()
	err = tx.QueryRow(ctx, `SELECT enqueued_at_ns FROM recall_async_semantic_jobs WHERE request_id = $1`, job.RequestID).Scan(&enqueuedAt)
	if err != nil && err != pgx.ErrNoRows {
		return port.AsyncSemanticReceipt{}, false, err
	}
	if err == pgx.ErrNoRows {
		if _, err := tx.Exec(ctx,
			`INSERT INTO recall_async_semantic_jobs(request_id, runtime_id, user_id, status, enqueued_at_ns, lease_until_ns, lease_token, attempt, failure_class, failure_err, result_json, payload_json)
			 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			job.RequestID, runtimeID, userID, sqlstmt.StatusPending, enqueuedAt, nil, "", 0, "", "", "", raw); err != nil {
			return port.AsyncSemanticReceipt{}, false, err
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE recall_async_semantic_jobs
			SET runtime_id = $1, user_id = $2, status = $3, lease_until_ns = NULL, lease_token = '', attempt = 0, failure_class = '', failure_err = '', result_json = '', payload_json = $4
			WHERE request_id = $5`,
			runtimeID, userID, sqlstmt.StatusPending, raw, job.RequestID); err != nil {
			return port.AsyncSemanticReceipt{}, false, err
		}
	}
	if err := q.replaceEpisodeRowsTx(ctx, tx, job); err != nil {
		return port.AsyncSemanticReceipt{}, false, err
	}
	depth, err := q.pendingDepthTx(ctx, tx, job.Scope)
	if err != nil {
		return port.AsyncSemanticReceipt{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return port.AsyncSemanticReceipt{}, false, err
	}
	return port.AsyncSemanticReceipt{RequestID: job.RequestID, EnqueuedAt: time.Unix(0, enqueuedAt).UTC(), QueueDepth: depth}, true, nil
}

func (q *asyncSemanticQueue) Cancel(ctx context.Context, requestID string) error {
	if requestID == "" {
		return nil
	}
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runtimeID, userID, status string
	err = tx.QueryRow(ctx, `SELECT runtime_id, user_id, status FROM recall_async_semantic_jobs WHERE request_id = $1`, requestID).Scan(&runtimeID, &userID, &status)
	if err == pgx.ErrNoRows || status == sqlstmt.StatusComplete {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if err := q.deleteRequestTx(ctx, tx, requestID); err != nil {
		return err
	}
	if err := incrementCounter(ctx, tx, sqlstmt.CounterAsyncSemantic, sqlstmt.ScopeFromParts(runtimeID, userID), 1); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (q *asyncSemanticQueue) CancelScope(ctx context.Context, scope domain.Scope) (int, error) {
	if scope.PartitionKey() == "" {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT request_id FROM recall_async_semantic_jobs WHERE runtime_id = $1 AND user_id = $2 AND status <> $3`, runtimeID, userID, sqlstmt.StatusComplete)
	if err != nil {
		return 0, err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	if err := q.deleteRequestsTx(ctx, tx, ids); err != nil {
		return 0, err
	}
	if err := incrementCounter(ctx, tx, sqlstmt.CounterAsyncSemantic, scope, len(ids)); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (q *asyncSemanticQueue) PurgeScope(ctx context.Context, scope domain.Scope) (int, error) {
	if scope.PartitionKey() == "" {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT request_id FROM recall_async_semantic_jobs WHERE runtime_id = $1 AND user_id = $2`, runtimeID, userID)
	if err != nil {
		return 0, err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	if err := q.deleteRequestsTx(ctx, tx, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (q *asyncSemanticQueue) CancelMatchingEpisodes(ctx context.Context, scope domain.Scope, deletedEpisodeFactIDs []string) (int, error) {
	if scope.PartitionKey() == "" || len(deletedEpisodeFactIDs) == 0 {
		return 0, nil
	}
	targets := sqlstmt.UniqueNonEmpty(deletedEpisodeFactIDs)
	if len(targets) == 0 {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	args := []any{runtimeID, userID, sqlstmt.StatusComplete}
	for _, id := range targets {
		args = append(args, id)
	}
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx,
		fmt.Sprintf(`SELECT DISTINCT j.request_id FROM recall_async_semantic_jobs j
			JOIN recall_async_semantic_job_episodes e ON e.request_id = j.request_id
			WHERE j.runtime_id = $1 AND j.user_id = $2 AND j.status <> $3 AND e.episode_fact_id IN (%s)`, phs(4, len(targets))),
		args...)
	if err != nil {
		return 0, err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	if err := q.deleteRequestsTx(ctx, tx, ids); err != nil {
		return 0, err
	}
	if err := incrementCounter(ctx, tx, sqlstmt.CounterAsyncSemantic, scope, len(ids)); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (q *asyncSemanticQueue) Claim(ctx context.Context, opts port.AsyncSemanticClaimOptions) ([]port.AsyncSemanticJob, error) {
	if opts.Max <= 0 {
		return nil, nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE recall_async_semantic_jobs SET status = $1, lease_token = '' WHERE status = $2 AND lease_until_ns IS NOT NULL AND lease_until_ns <= $3`, sqlstmt.StatusPending, sqlstmt.StatusLeased, now.UnixNano()); err != nil {
		return nil, err
	}
	where, args := q.claimWhere(opts, now)
	args = append(args, opts.Max)
	rows, err := tx.Query(ctx,
		fmt.Sprintf(`SELECT request_id, payload_json FROM recall_async_semantic_jobs WHERE %s ORDER BY enqueued_at_ns ASC, runtime_id ASC, user_id ASC, request_id ASC LIMIT %s FOR UPDATE SKIP LOCKED`, where, ph(len(args))),
		args...)
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
	out := make([]port.AsyncSemanticJob, 0, len(selected))
	for _, r := range selected {
		job, err := sqlstmt.DecodeJSON[port.AsyncSemanticJob](r.raw)
		if err != nil {
			return nil, err
		}
		job.Attempt++
		job.LeaseUntil = now.Add(sqlstmt.AsyncSemanticLeaseTTL)
		job.LeaseToken = newLeaseToken()
		raw, err := sqlstmt.EncodeJSON(job)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE recall_async_semantic_jobs SET status = $1, lease_until_ns = $2, lease_token = $3, attempt = $4, payload_json = $5 WHERE request_id = $6 AND status = $7`,
			sqlstmt.StatusLeased, job.LeaseUntil.UnixNano(), job.LeaseToken, job.Attempt, raw, r.id, sqlstmt.StatusPending); err != nil {
			return nil, err
		}
		out = append(out, port.CloneAsyncSemanticJob(job))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	_ = opts.WorkerID
	return out, nil
}

func (q *asyncSemanticQueue) Complete(ctx context.Context, requestID, leaseToken string, result port.AsyncSemanticResult) error {
	if requestID == "" {
		return nil
	}
	return q.mutateLeased(ctx, requestID, leaseToken, func(job *port.AsyncSemanticJob) (string, port.AsyncSemanticFailure, string, error) {
		job.LeaseUntil = time.Time{}
		job.LeaseToken = ""
		port.ScrubAsyncSemanticJobPII(job)
		rawResult, err := sqlstmt.EncodeJSON(result)
		return sqlstmt.StatusComplete, port.AsyncSemanticFailure{}, rawResult, err
	})
}

func (q *asyncSemanticQueue) Fail(ctx context.Context, requestID, leaseToken string, failure port.AsyncSemanticFailure) error {
	if requestID == "" {
		return nil
	}
	return q.mutateLeased(ctx, requestID, leaseToken, func(job *port.AsyncSemanticJob) (string, port.AsyncSemanticFailure, string, error) {
		job.LeaseUntil = time.Time{}
		job.LeaseToken = ""
		if failure.ErrClass == diagnostic.ErrClassPermanent {
			port.ScrubAsyncSemanticJobPII(job)
			return sqlstmt.StatusFailed, failure, "", nil
		}
		return sqlstmt.StatusPending, failure, "", nil
	})
}

func (q *asyncSemanticQueue) Stats(ctx context.Context, filter port.AsyncSemanticStatsFilter) (port.AsyncSemanticStats, error) {
	if filter.Scope.PartitionKey() == "" {
		return port.AsyncSemanticStats{}, errdefs.Validationf("asyncsemantic: scope partition is required (RuntimeID and UserID)")
	}
	now := filter.Now
	if now.IsZero() {
		now = time.Now()
	}
	runtimeID, userID := sqlstmt.ScopeParts(filter.Scope)
	rows, err := q.b.pool.Query(ctx, `SELECT status, lease_until_ns, failure_class FROM recall_async_semantic_jobs WHERE runtime_id = $1 AND user_id = $2`, runtimeID, userID)
	if err != nil {
		return port.AsyncSemanticStats{}, err
	}
	defer rows.Close()
	var out port.AsyncSemanticStats
	for rows.Next() {
		var status, failureClass string
		var leaseUntil *int64
		if err := rows.Scan(&status, &leaseUntil, &failureClass); err != nil {
			return port.AsyncSemanticStats{}, err
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
		return port.AsyncSemanticStats{}, err
	}
	out.CancelledTotal, err = counter(ctx, q.b.pool, sqlstmt.CounterAsyncSemantic, filter.Scope)
	return out, err
}

func (q *asyncSemanticQueue) replaceEpisodeRowsTx(ctx context.Context, tx pgx.Tx, job port.AsyncSemanticJob) error {
	if _, err := tx.Exec(ctx, `DELETE FROM recall_async_semantic_job_episodes WHERE request_id = $1`, job.RequestID); err != nil {
		return err
	}
	runtimeID, userID := sqlstmt.ScopeParts(job.Scope)
	for _, id := range sqlstmt.UniqueNonEmpty(job.EpisodeFactIDs) {
		if _, err := tx.Exec(ctx, `INSERT INTO recall_async_semantic_job_episodes(request_id, runtime_id, user_id, episode_fact_id) VALUES($1,$2,$3,$4)`, job.RequestID, runtimeID, userID, id); err != nil {
			return err
		}
	}
	return nil
}

func (q *asyncSemanticQueue) pendingDepthTx(ctx context.Context, tx pgx.Tx, scope domain.Scope) (int, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var n int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM recall_async_semantic_jobs WHERE runtime_id = $1 AND user_id = $2 AND status = $3`, runtimeID, userID, sqlstmt.StatusPending).Scan(&n)
	return n, err
}

func (q *asyncSemanticQueue) deleteRequestTx(ctx context.Context, tx pgx.Tx, requestID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM recall_async_semantic_job_episodes WHERE request_id = $1`, requestID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM recall_async_semantic_jobs WHERE request_id = $1`, requestID)
	return err
}

func (q *asyncSemanticQueue) deleteRequestsTx(ctx context.Context, tx pgx.Tx, requestIDs []string) error {
	for _, id := range requestIDs {
		if err := q.deleteRequestTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func (q *asyncSemanticQueue) claimWhere(opts port.AsyncSemanticClaimOptions, now time.Time) (string, []any) {
	args := []any{sqlstmt.StatusPending, now.UnixNano()}
	where := `status = $1 AND (lease_until_ns IS NULL OR lease_until_ns <= $2)`
	if opts.Scope != nil {
		runtimeID, userID := sqlstmt.ScopeParts(*opts.Scope)
		args = append(args, runtimeID, userID)
		where += ` AND runtime_id = $3 AND user_id = $4`
		return where, args
	}
	if opts.RuntimeID != "" {
		args = append(args, opts.RuntimeID)
		where += ` AND runtime_id = $3`
	}
	return where, args
}

func (q *asyncSemanticQueue) mutateLeased(ctx context.Context, requestID, leaseToken string, mutate func(*port.AsyncSemanticJob) (string, port.AsyncSemanticFailure, string, error)) error {
	tx, err := q.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status, currentToken, raw string
	err = tx.QueryRow(ctx, `SELECT status, lease_token, payload_json FROM recall_async_semantic_jobs WHERE request_id = $1`, requestID).Scan(&status, &currentToken, &raw)
	if err == pgx.ErrNoRows || status == sqlstmt.StatusComplete {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if status != sqlstmt.StatusLeased || leaseToken == "" || currentToken != leaseToken {
		return tx.Commit(ctx)
	}
	job, err := sqlstmt.DecodeJSON[port.AsyncSemanticJob](raw)
	if err != nil {
		return err
	}
	nextStatus, failure, resultRaw, err := mutate(&job)
	if err != nil {
		return err
	}
	if nextStatus == sqlstmt.StatusPending && job.Attempt >= sqlstmt.AsyncMaxAttempts {
		nextStatus = sqlstmt.StatusFailed
		failure.ErrClass = diagnostic.ErrClassPermanent
		failure.RetryAt = time.Time{}
		port.ScrubAsyncSemanticJobPII(&job)
	}
	var leaseUntil any
	if nextStatus == sqlstmt.StatusPending {
		retryAt := failure.RetryAt
		now := time.Now()
		if retryAt.IsZero() || !retryAt.After(now) {
			retryAt = now.Add(sqlstmt.AsyncRetryBackoff)
		}
		job.LeaseUntil = retryAt
		leaseUntil = retryAt.UnixNano()
	}
	raw, err = sqlstmt.EncodeJSON(job)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE recall_async_semantic_jobs SET status = $1, lease_until_ns = $2, lease_token = '', failure_class = $3, failure_err = $4, result_json = $5, payload_json = $6 WHERE request_id = $7`,
		nextStatus, leaseUntil, string(failure.ErrClass), failure.Err, resultRaw, raw, requestID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ port.AsyncSemanticQueue = (*asyncSemanticQueue)(nil)
