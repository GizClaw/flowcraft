package memory

import (
	"context"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	postgresstore "github.com/GizClaw/flowcraft/backends/memory/storage/postgres"
	sqlitestore "github.com/GizClaw/flowcraft/backends/memory/storage/sqlite"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

type sqliteDriverSettings struct {
	Path string `json:"path"`
}

type postgresDriverSettings struct {
	DSN    string `json:"dsn"`
	Schema string `json:"schema,omitempty"`
}

// openStorage resolves the configured Log and KV drivers. SQLite drivers
// pointing at the same path share one connection pool. A failure closes every
// pool opened so far instead of leaking it.
func openStorage(
	ctx context.Context,
	settings StorageSettings,
	ws workspace.Workspace,
) (storage.Log, storage.Store, []io.Closer, error) {
	var opened []io.Closer
	logStore, kvStore, err := openStores(ctx, settings, ws, &opened)
	if err != nil {
		for index := len(opened) - 1; index >= 0; index-- {
			_ = opened[index].Close()
		}
		return nil, nil, nil, err
	}
	return logStore, kvStore, opened, nil
}

// openStores opens the configured drivers, recording every pool that must be
// closed on success and on failure.
func openStores(
	ctx context.Context,
	settings StorageSettings,
	ws workspace.Workspace,
	opened *[]io.Closer,
) (storage.Log, storage.Store, error) {
	var shared *sqlitestore.Store
	var sharedPG *postgresstore.Store
	postgresKey := func(config postgresDriverSettings) string {
		return config.DSN + "\x00" + config.Schema
	}
	openSQLite := func(driver DriverSettings, name string) (*sqlitestore.Store, error) {
		config, err := resource.DecodeTyped[sqliteDriverSettings](ctx, driver.Settings)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf("memory config: %s settings: %w", name, err))
		}
		if config.Path == "" {
			return nil, errdefs.Validationf("memory config: %s.settings.path is required", name)
		}
		if shared != nil && shared.Path() == config.Path {
			return shared, nil
		}
		store, err := sqlitestore.Open(ctx, config.Path)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf("memory config: %s open: %w", name, err))
		}
		*opened = append(*opened, store)
		if shared == nil {
			shared = store
		}
		return store, nil
	}
	openPostgres := func(driver DriverSettings, name string) (*postgresstore.Store, error) {
		config, err := resource.DecodeTyped[postgresDriverSettings](ctx, driver.Settings)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf("memory config: %s settings: %w", name, err))
		}
		if config.DSN == "" {
			return nil, errdefs.Validationf("memory config: %s.settings.dsn is required", name)
		}
		if sharedPG != nil && postgresKey(config) == sharedPG.DSNKey() {
			return sharedPG, nil
		}
		store, err := postgresstore.Open(ctx, config.DSN, config.Schema)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf("memory config: %s open: %w", name, err))
		}
		*opened = append(*opened, store)
		if sharedPG == nil {
			sharedPG = store
		}
		return store, nil
	}
	var logStore storage.Log
	switch settings.Log.Driver {
	case DriverWorkspace:
		value, err := storage.NewWorkspaceLog(ws)
		if err != nil {
			return nil, nil, errdefs.Validation(fmt.Errorf("memory config: open log: %w", err))
		}
		logStore = value
	case DriverSQLite:
		value, err := openSQLite(settings.Log, "storage.log")
		if err != nil {
			return nil, nil, err
		}
		logStore = value
	case DriverPostgres:
		value, err := openPostgres(settings.Log, "storage.log")
		if err != nil {
			return nil, nil, err
		}
		logStore = value
	default:
		return nil, nil, errdefs.Validationf("memory config: storage.log.driver %q is not supported", settings.Log.Driver)
	}
	var kvStore storage.Store
	switch settings.KV.Driver {
	case DriverWorkspace:
		value, err := storage.NewWorkspaceKV(ws)
		if err != nil {
			return nil, nil, errdefs.Validation(fmt.Errorf("memory config: open kv: %w", err))
		}
		kvStore = value
	case DriverSQLite:
		value, err := openSQLite(settings.KV, "storage.kv")
		if err != nil {
			return nil, nil, err
		}
		kvStore = value
	case DriverPostgres:
		value, err := openPostgres(settings.KV, "storage.kv")
		if err != nil {
			return nil, nil, err
		}
		kvStore = value
	default:
		return nil, nil, errdefs.Validationf("memory config: storage.kv.driver %q is not supported", settings.KV.Driver)
	}
	return logStore, kvStore, nil
}
