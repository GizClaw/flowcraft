package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
)

func newLeaseToken() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func scanStrings(rows *sql.Rows) ([]string, error) {
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

func incrementCounter(ctx context.Context, tx *sql.Tx, kind string, scope domain.Scope, n int) error {
	if n <= 0 {
		return nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	res, err := tx.ExecContext(ctx,
		`UPDATE recall_queue_counters SET cancelled_total = cancelled_total + ? WHERE kind = ? AND runtime_id = ? AND user_id = ?`,
		n, kind, runtimeID, userID)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO recall_queue_counters(kind, runtime_id, user_id, cancelled_total) VALUES(?,?,?,?)`,
		kind, runtimeID, userID, n)
	return err
}

func counter(ctx context.Context, db *sql.DB, kind string, scope domain.Scope) (int, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT cancelled_total FROM recall_queue_counters WHERE kind = ? AND runtime_id = ? AND user_id = ?`,
		kind, runtimeID, userID).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}
