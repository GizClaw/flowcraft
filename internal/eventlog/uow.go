package eventlog

import (
	"context"
	"database/sql"
)

// UnitOfWork is the transaction-scoped context for business operations + event appends.
// All business SQL and event appends must go through this interface;
// raw *sql.Tx must never escape the implementation.
type UnitOfWork interface {
	// BusinessExec runs a business SQL statement within the same transaction.
	BusinessExec(ctx context.Context, query string, args ...any) (sql.Result, error)
	// BusinessQuery runs a business SQL query within the same transaction.
	BusinessQuery(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	// BusinessQueryRow runs a business SQL query returning a single row.
	BusinessQueryRow(ctx context.Context, query string, args ...any) *sql.Row

	// Append appends one or more envelope drafts to the log within the same transaction.
	// The drafts' order is preserved as the append order.
	Append(ctx context.Context, drafts ...EnvelopeDraft) error

	// CheckpointSet updates a projector checkpoint within the same transaction.
	CheckpointSet(ctx context.Context, projectorName string, seq int64) error

	// Sequence returns the sequence number that will be assigned to the next Append.
	Sequence() int64
}
