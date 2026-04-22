package bootstrap

import (
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	"github.com/GizClaw/flowcraft/internal/store"
)

// WireProjectionManager constructs the per-process ProjectorManager with a
// SQLite-backed dead-letter sink so all projectors write to dead_letters on
// exhaustion rather than just logging.
func WireProjectionManager(st *store.SQLiteStore) *projection.Manager {
	cfg := projection.ManagerConfig{
		DLTSink: projection.NewSQLiteDLT(st.DB()),
	}
	return projection.NewManager(cfg)
}
