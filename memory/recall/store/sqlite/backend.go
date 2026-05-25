package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"

	_ "modernc.org/sqlite"
)

// Backend owns the SQLite connection shared by recall's durable adapters.
type Backend struct {
	db *sql.DB

	closeOnce sync.Once
	closeErr  error
}

// Store is the durable canonical ledger plus its optional scope enumerator.
type Store interface {
	recall.TemporalStore
	recall.ScopeEnumerator
}

// Open opens or creates a SQLite-backed recall durable backend.
//
// Use ":memory:" for an in-process database.
func Open(ctx context.Context, path string) (*Backend, error) {
	if path == "" {
		return nil, errdefs.Validationf("recall sqlite: path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, errdefs.NotAvailable(fmt.Errorf("recall sqlite: %s: %w", pragma, err))
		}
	}
	for _, stmt := range sqlstmt.Schema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, errdefs.NotAvailable(fmt.Errorf("recall sqlite migrate: %w", err))
		}
	}
	return &Backend{db: db}, nil
}

// Close closes the shared SQLite connection.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.db.Close()
	})
	return b.closeErr
}

// TemporalStore returns the canonical fact ledger adapter.
func (b *Backend) TemporalStore() Store {
	return &temporalStore{b: b}
}

// SideEffectOutbox returns the commit-after side-effect outbox adapter.
func (b *Backend) SideEffectOutbox() recall.SideEffectOutbox {
	return &sideEffectOutbox{b: b}
}

// AsyncSemanticQueue returns the async semantic durable queue adapter.
func (b *Backend) AsyncSemanticQueue() recall.AsyncSemanticQueue {
	return &asyncSemanticQueue{b: b}
}

func ph(int) string { return "?" }

func phs(start, n int) string { return sqlstmt.Placeholders(start, n, false) }
