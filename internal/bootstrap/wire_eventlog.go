package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/store"
)

// WireEventLog returns a SQLiteLog backed by the shared store DB. Migrations
// 006+ are run by the store on open, so the table set is already in place.
func WireEventLog(_ context.Context, st *store.SQLiteStore) (*eventlog.SQLiteLog, error) {
	return eventlog.NewSQLiteLog(st.DB()), nil
}
