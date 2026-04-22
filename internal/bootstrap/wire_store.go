package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/store"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// wireStore opens the SQLite database and optionally wraps it with tracing.
// The returned cleanup closes the store.
func wireStore(ctx context.Context, cfg *config.Config) (model.Store, *store.SQLiteStore, func(), error) {
	dbPath := cfg.DBPath()
	sqliteStore, err := store.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		telemetry.Error(ctx, "failed to open database",
			otellog.String("path", dbPath), otellog.String("error", err.Error()))
		return nil, nil, nil, err
	}
	var appStore model.Store = sqliteStore
	if cfg.Telemetry.Enabled {
		appStore = store.WithStoreTracing(appStore)
	}
	cleanup := func() { _ = appStore.Close() }
	return appStore, sqliteStore, cleanup, nil
}
