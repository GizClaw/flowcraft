package postgres

import (
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage/internal/storagetest"
)

// TestPostgresDriverConformance runs the shared Log/KV contract suite against
// the PostgreSQL driver. It self-skips without FC_PG_DSN.
func TestPostgresDriverConformance(t *testing.T) {
	store := newTestStore(t)
	storagetest.Run(t, storagetest.Backend{Log: store, KV: store})
}
