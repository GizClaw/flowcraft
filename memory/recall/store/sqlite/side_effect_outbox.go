package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
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
	tx, err := q.b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM recall_side_effect_jobs WHERE id = ?`, job.ID).Scan(&one)
	if err == nil {
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO recall_side_effect_jobs(id, request_id, runtime_id, user_id, kind, status, enqueued_at_ns, retry_at_ns, lease_until_ns, lease_token, attempt, failure_class, failure_err, result_json, payload_json) VALUES(%s)`, phs(1, 15)),
		job.ID, job.RequestID, runtimeID, userID, string(job.Kind), sqlstmt.StatusPending, time.Now().UnixNano(), nil, nil, "", 0, "", "", "", raw)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (q *sideEffectOutbox) Claim(ctx context.Context, opts port.SideEffectClaimOptions) ([]port.SideEffectJob, error) {
	if opts.Max <= 0 {
		return nil, nil
	}
	if opts.Scope.PartitionKey() == "" {
		return nil, nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	runtimeID, userID := sqlstmt.ScopeParts(opts.Scope)
	tx, err := q.b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE recall_side_effect_jobs SET status = ?, lease_until_ns = NULL, lease_token = '' WHERE status = ? AND lease_until_ns IS NOT NULL AND lease_until_ns <= ?`, sqlstmt.StatusPending, sqlstmt.StatusLeased, now.UnixNano()); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, payload_json FROM recall_side_effect_jobs
			WHERE runtime_id = ? AND user_id = ? AND status = ? AND (retry_at_ns IS NULL OR retry_at_ns <= ?)
			ORDER BY enqueued_at_ns ASC, id ASC LIMIT %d`, opts.Max),
		runtimeID, userID, sqlstmt.StatusPending, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type selectedRow struct{ id, raw string }
	selected := make([]selectedRow, 0)
	for rows.Next() {
		var r selectedRow
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			return nil, err
		}
		selected = append(selected, r)
	}
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
		if _, err := tx.ExecContext(ctx,
			`UPDATE recall_side_effect_jobs SET status = ?, lease_until_ns = ?, lease_token = ?, attempt = ?, retry_at_ns = NULL, payload_json = ? WHERE id = ? AND status = ?`,
			sqlstmt.StatusLeased, job.LeaseUntil.UnixNano(), job.LeaseToken, job.Attempt, raw, r.id, sqlstmt.StatusPending); err != nil {
			return nil, err
		}
		out = append(out, sqlstmt.CloneSideEffectJob(job))
	}
	if err := tx.Commit(); err != nil {
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
	tx, err := q.b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, runtime_id, user_id FROM recall_side_effect_jobs WHERE request_id = ? AND status <> ?`, requestID, sqlstmt.StatusComplete)
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
		if _, err := tx.ExecContext(ctx, `DELETE FROM recall_side_effect_jobs WHERE id = ?`, r.id); err != nil {
			return err
		}
		if err := incrementCounter(ctx, tx, sqlstmt.CounterSideEffect, sqlstmt.ScopeFromParts(r.runtimeID, r.userID), 1); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (q *sideEffectOutbox) CancelScope(ctx context.Context, scope domain.Scope) (int, error) {
	if scope.PartitionKey() == "" {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tx, err := q.b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM recall_side_effect_jobs WHERE runtime_id = ? AND user_id = ? AND status <> ?`, runtimeID, userID, sqlstmt.StatusComplete)
	if err != nil {
		return 0, err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM recall_side_effect_jobs WHERE id = ?`, id); err != nil {
			return 0, err
		}
	}
	if err := incrementCounter(ctx, tx, sqlstmt.CounterSideEffect, scope, len(ids)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (q *sideEffectOutbox) PurgeScope(ctx context.Context, scope domain.Scope) (int, error) {
	if scope.PartitionKey() == "" {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tx, err := q.b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recall_side_effect_jobs WHERE runtime_id = ? AND user_id = ?`, runtimeID, userID).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM recall_side_effect_jobs WHERE runtime_id = ? AND user_id = ?`, runtimeID, userID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
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
	rows, err := q.b.db.QueryContext(ctx, `SELECT status, lease_until_ns, failure_class FROM recall_side_effect_jobs WHERE runtime_id = ? AND user_id = ?`, runtimeID, userID)
	if err != nil {
		return port.SideEffectStats{}, err
	}
	defer rows.Close()
	var out port.SideEffectStats
	for rows.Next() {
		var status, failureClass string
		var leaseUntil sql.NullInt64
		if err := rows.Scan(&status, &leaseUntil, &failureClass); err != nil {
			return port.SideEffectStats{}, err
		}
		switch status {
		case sqlstmt.StatusPending:
			out.Pending++
		case sqlstmt.StatusLeased:
			out.Leased++
			if leaseUntil.Valid && leaseUntil.Int64 <= now.UnixNano() {
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
	out.CancelledTotal, err = counter(ctx, q.b.db, sqlstmt.CounterSideEffect, scope)
	return out, err
}

func (q *sideEffectOutbox) mutateLeased(ctx context.Context, jobID, leaseToken string, mutate func(*port.SideEffectJob) (string, port.SideEffectFailure, string, error)) error {
	tx, err := q.b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, currentToken, raw string
	err = tx.QueryRowContext(ctx, `SELECT status, lease_token, payload_json FROM recall_side_effect_jobs WHERE id = ?`, jobID).Scan(&status, &currentToken, &raw)
	if err == sql.ErrNoRows || status == sqlstmt.StatusComplete {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if status != sqlstmt.StatusLeased || leaseToken == "" || currentToken != leaseToken {
		return tx.Commit()
	}
	job, err := sqlstmt.DecodeJSON[port.SideEffectJob](raw)
	if err != nil {
		return err
	}
	nextStatus, failure, resultRaw, err := mutate(&job)
	if err != nil {
		return err
	}
	retryAt := any(nil)
	if nextStatus == sqlstmt.StatusPending && !failure.RetryAt.IsZero() {
		retryAt = failure.RetryAt.UnixNano()
	}
	raw, err = sqlstmt.EncodeJSON(job)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE recall_side_effect_jobs SET status = ?, retry_at_ns = ?, lease_until_ns = NULL, lease_token = '', failure_class = ?, failure_err = ?, result_json = ?, payload_json = ? WHERE id = ?`,
		nextStatus, retryAt, string(failure.ErrClass), failure.Err, resultRaw, raw, jobID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

var _ port.SideEffectOutbox = (*sideEffectOutbox)(nil)
