package postgres

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Backend owns the PostgreSQL connection shared by recall's durable adapters.
type Backend struct {
	pool *pgxpool.Pool
}

// Store is the durable canonical ledger plus its optional scope enumerator.
type Store interface {
	recall.TemporalStore
	recall.ScopeEnumerator
}

// Open creates a PostgreSQL-backed recall durable backend from a DSN.
func Open(ctx context.Context, dsn string) (*Backend, error) {
	if dsn == "" {
		return nil, errdefs.Validationf("recall postgres: dsn is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	for _, stmt := range sqlstmt.Schema {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			pool.Close()
			return nil, errdefs.NotAvailable(err)
		}
	}
	return &Backend{pool: pool}, nil
}

// Close closes the shared PostgreSQL connection.
func (b *Backend) Close() error {
	b.pool.Close()
	return nil
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

// EvidenceStore returns the secondary evidence lookup adapter.
func (b *Backend) EvidenceStore() recall.EvidenceStore {
	return &evidenceStore{b: b}
}

// ObservationStore returns the canonical raw-evidence graph adapter.
func (b *Backend) ObservationStore() recall.ObservationStore {
	return &observationStore{b: b}
}

// LinkStore returns the canonical graph link adapter.
func (b *Backend) LinkStore() recall.LinkStore {
	return &linkStore{b: b}
}

func ph(n int) string { return sqlstmt.Placeholders(n, 1, true) }

func phs(start, n int) string { return sqlstmt.Placeholders(start, n, true) }
