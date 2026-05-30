package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newLeaseToken() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func scanStrings(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func incrementCounter(ctx context.Context, tx pgx.Tx, kind string, scope domain.Scope, n int) error {
	if n <= 0 {
		return nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tag, err := tx.Exec(ctx,
		`UPDATE recall_queue_counters SET cancelled_total = cancelled_total + $1 WHERE kind = $2 AND runtime_id = $3 AND user_id = $4`,
		n, kind, runtimeID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO recall_queue_counters(kind, runtime_id, user_id, cancelled_total) VALUES($1,$2,$3,$4)`,
		kind, runtimeID, userID, n)
	return err
}

func counter(ctx context.Context, pool *pgxpool.Pool, kind string, scope domain.Scope) (int, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var n int
	err := pool.QueryRow(ctx,
		`SELECT cancelled_total FROM recall_queue_counters WHERE kind = $1 AND runtime_id = $2 AND user_id = $3`,
		kind, runtimeID, userID).Scan(&n)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	return n, err
}
