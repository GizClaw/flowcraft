package sqlite

import (
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage/internal/storagetest"
)

// TestSQLiteDriverConformance runs the shared Log/KV contract suite against
// the SQLite driver.
func TestSQLiteDriverConformance(t *testing.T) {
	store := newTestStore(t)
	storagetest.Run(t, storagetest.Backend{Log: store, KV: store})
}
