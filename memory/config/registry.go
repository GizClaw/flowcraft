package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// BackendSettings selects one storage driver and carries its driver-owned
// settings. The sdk/memory protocol never hard-codes driver names.
type BackendSettings struct {
	Driver   string          `json:"driver"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// StorageSettings is the declarative storage section of memory.yaml. The
// composition-root path injects Backends directly and must not also provide
// this section.
type StorageSettings struct {
	Log BackendSettings `json:"log,omitempty"`
	KV  BackendSettings `json:"kv,omitempty"`
	// Search selects the SearchBackend driver per retrieval lane. The read
	// path still consumes component.Searcher plugin lanes; this section
	// closes the backend contract so OpenSearch can later replace lanes.
	Search SearchSettings `json:"search,omitempty"`
}

// SearchSettings declares one backend per lane name.
type SearchSettings struct {
	Lanes map[string]BackendSettings `json:"lanes,omitempty"`
}

// IsEmpty reports whether no driver is selected.
func (settings StorageSettings) IsEmpty() bool {
	return settings.Log.Driver == "" && settings.KV.Driver == "" && len(settings.Search.Lanes) == 0
}

type logDriverFactory func(json.RawMessage) (storage.Log, error)
type kvDriverFactory func(json.RawMessage) (storage.Store, error)
type searchDriverFactory func(SearchDriverDeps, json.RawMessage) (retrieval.SearchBackend, error)

// SearchDriverDeps carries what a search driver needs beyond its settings:
// the KV backend (for future index storage) and the lane it wraps.
type SearchDriverDeps struct {
	KV   storage.Store
	Lane component.Searcher
}

// DriverRegistry resolves declarative storage sections into backend
// instances. Registration is instance-owned, mirroring inference factories.
type DriverRegistry struct {
	logDrivers map[string]logDriverFactory
	kvDrivers  map[string]kvDriverFactory
	search     map[string]searchDriverFactory
}

// NewDriverRegistry returns an empty registry.
func NewDriverRegistry() *DriverRegistry {
	return &DriverRegistry{
		logDrivers: make(map[string]logDriverFactory),
		kvDrivers:  make(map[string]kvDriverFactory),
		search:     make(map[string]searchDriverFactory),
	}
}

// RegisterLogDriver registers one log backend driver by name.
func (registry *DriverRegistry) RegisterLogDriver(name string, factory func(json.RawMessage) (storage.Log, error)) error {
	if strings.TrimSpace(name) == "" || factory == nil {
		return errors.New("memory config: log driver name and factory are required")
	}
	if _, exists := registry.logDrivers[name]; exists {
		return fmt.Errorf("memory config: log driver %q is already registered", name)
	}
	registry.logDrivers[name] = factory
	return nil
}

// RegisterKVDriver registers one store backend driver by name.
func (registry *DriverRegistry) RegisterKVDriver(name string, factory func(json.RawMessage) (storage.Store, error)) error {
	if strings.TrimSpace(name) == "" || factory == nil {
		return errors.New("memory config: store driver name and factory are required")
	}
	if _, exists := registry.kvDrivers[name]; exists {
		return fmt.Errorf("memory config: store driver %q is already registered", name)
	}
	registry.kvDrivers[name] = factory
	return nil
}

// RegisterSearchDriver registers one search backend driver by name.
func (registry *DriverRegistry) RegisterSearchDriver(
	name string,
	factory func(SearchDriverDeps, json.RawMessage) (retrieval.SearchBackend, error),
) error {
	if strings.TrimSpace(name) == "" || factory == nil {
		return errors.New("memory config: search driver name and factory are required")
	}
	if _, exists := registry.search[name]; exists {
		return fmt.Errorf("memory config: search driver %q is already registered", name)
	}
	registry.search[name] = factory
	return nil
}

// RegisterWorkspaceBackends binds the "workspace" log and store drivers to one
// workspace instance. It mirrors s3.Register(builder, client): the instance is
// a Go value a document cannot name.
func RegisterWorkspaceBackends(registry *DriverRegistry, ws workspace.Workspace) error {
	if registry == nil || nilInterface(ws) {
		return errors.New("memory config: registry and workspace are required")
	}
	if err := registry.RegisterLogDriver("workspace", func(json.RawMessage) (storage.Log, error) {
		return storage.NewWorkspaceLog(ws)
	}); err != nil {
		return err
	}
	return registry.RegisterKVDriver("workspace", func(json.RawMessage) (storage.Store, error) {
		return storage.NewWorkspaceKV(ws)
	})
}

// Resolve builds one Backends from a declarative storage section. Both sides
// must select a registered driver; the section must not be empty.
func (registry *DriverRegistry) Resolve(settings StorageSettings) (Backends, error) {
	if registry == nil || (settings.Log.Driver == "" && settings.KV.Driver == "") {
		return Backends{}, errdefs.Validationf("memory config: storage log/kv drivers are required")
	}
	logFactory, ok := registry.logDrivers[settings.Log.Driver]
	if !ok {
		return Backends{}, errdefs.Validationf("memory config: log driver %q is not registered", settings.Log.Driver)
	}
	kvFactory, ok := registry.kvDrivers[settings.KV.Driver]
	if !ok {
		return Backends{}, errdefs.Validationf("memory config: store driver %q is not registered", settings.KV.Driver)
	}
	logStore, err := logFactory(settings.Log.Settings)
	if err != nil {
		return Backends{}, errdefs.Validationf("memory config: build log driver %q: %v", settings.Log.Driver, err)
	}
	kvStore, err := kvFactory(settings.KV.Settings)
	if err != nil {
		return Backends{}, errdefs.Validationf("memory config: build store driver %q: %v", settings.KV.Driver, err)
	}
	return Backends{Log: logStore, KV: kvStore}, nil
}

// ResolveSearchLanes builds one SearchBackend per lane from the declarative
// search section. When the section is empty it returns an empty map; the
// caller applies its default lanes.
func (registry *DriverRegistry) ResolveSearchLanes(
	settings SearchSettings,
	lanes map[string]component.Searcher,
	kv storage.Store,
) (map[string]retrieval.SearchBackend, error) {
	if registry == nil {
		return nil, errors.New("memory config: search registry is required")
	}
	result := make(map[string]retrieval.SearchBackend, len(settings.Lanes))
	for laneName, backendSettings := range settings.Lanes {
		if strings.TrimSpace(backendSettings.Driver) == "" {
			return nil, errdefs.Validationf("memory config: search lane %q requires a driver", laneName)
		}
		factory, ok := registry.search[backendSettings.Driver]
		if !ok {
			return nil, errdefs.Validationf("memory config: search driver %q is not registered", backendSettings.Driver)
		}
		searcher, ok := lanes[laneName]
		if !ok {
			return nil, errdefs.Validationf("memory config: search lane %q is not a known lane", laneName)
		}
		backend, err := factory(SearchDriverDeps{KV: kv, Lane: searcher}, backendSettings.Settings)
		if err != nil {
			return nil, errdefs.Validationf("memory config: build search lane %q: %v", laneName, err)
		}
		result[laneName] = backend
	}
	return result, nil
}
