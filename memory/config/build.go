package config

import (
	"errors"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Backends are the canonical storage substrates injected by the composition
// root. They are borrowed: Assembly.Close never closes them. Search maps lane
// names to SearchBackend implementations; when empty, NewAssembly builds the
// default lsm LaneBackends for the built-in lanes.
type Backends struct {
	Log storage.Log
	KV  storage.Store
	// Search is optional: lane name -> SearchBackend. It must not be
	// combined with the declarative storage.search.lanes section.
	Search map[string]retrieval.SearchBackend
}

type Builder struct {
	log       storage.Log
	kv        storage.Store
	inference *inference.Runtime
	outbox    workspace.Workspace
	search    map[string]retrieval.SearchBackend
}

// NewBuilder binds injected canonical backends and the inference runtime. It
// starts no goroutines.
func NewBuilder(backends Backends, runtime *inference.Runtime) (*Builder, error) {
	if nilInterface(backends.Log) {
		return nil, errors.New("memory config: log backend is required")
	}
	if nilInterface(backends.KV) {
		return nil, errors.New("memory config: store backend is required")
	}
	if _, ok := backends.KV.(storage.PutIfAbsentStore); !ok {
		return nil, errors.New("memory config: store backend must support immutable writes")
	}
	if runtime == nil {
		return nil, errors.New("memory config: inference runtime is required")
	}
	builder := &Builder{log: backends.Log, kv: backends.KV, inference: runtime}
	if len(backends.Search) > 0 {
		builder.search = make(map[string]retrieval.SearchBackend, len(backends.Search))
		for name, backend := range backends.Search {
			builder.search[name] = backend
		}
	}
	return builder, nil
}

// WithOutboxWorkspace binds the workspace used only by the durable lifecycle
// outbox (a lease queue that stays workspace-backed; see plan §5.6). It is
// required only when Lifecycle is enabled.
func (b *Builder) WithOutboxWorkspace(ws workspace.Workspace) *Builder {
	b.outbox = ws
	return b
}

// LogStore returns the canonical log backend.
func (b *Builder) LogStore() storage.Log { return b.log }

// KVStore returns the canonical store backend.
func (b *Builder) KVStore() storage.Store { return b.kv }
