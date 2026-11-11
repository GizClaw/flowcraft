package deploy

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// EngineSpec selects an engine kind and carries its settings.
type EngineSpec struct {
	// Kind is required. There is no default implementation: an agent that does
	// not say what it is cannot be deployed meaningfully.
	Kind string
	// Settings is the kind's own configuration as written (JSON). The kind
	// decodes it, at load time for validation and again when it builds.
	Settings json.RawMessage
}

// ProtocolSpec selects how the agent is exposed.
type ProtocolSpec struct {
	// Kind is required when the block is present.
	Kind string
	// Settings is the protocol's own configuration as written (JSON).
	Settings json.RawMessage
}

// EngineDeps is what an engine kind is built from.
type EngineDeps struct {
	Spec        EngineSpec
	Runtime     *runtime.Runtime
	Store       agent.Store
	Fingerprint agent.Fingerprint
	// Manifest is the version combination this deployment records.
	Manifest agent.Manifest
	// Trace is the host's tracing policy for runs of this engine.
	Trace telemetry.TraceLevel
}

// ProtocolDeps is what a protocol kind is built from. It carries no deployment
// internals: name, store, identity and the session table are all it needs.
type ProtocolDeps struct {
	Spec        ProtocolSpec
	Name        string
	Store       agent.Store
	Fingerprint agent.Fingerprint
	ArtifactURL func(ref string) string
	// Agent is the deployment's session table: a protocol drives it directly.
	Agent *agent.Agent
}

// EngineKind registers one engine kind.
type EngineKind struct {
	Name string
	// Local marks an engine that runs in this process: it needs a runtime and
	// has no peer. A remote engine is the opposite, and the loader validates each
	// kind by what it says here instead of by its name.
	Local bool
	// Decode validates the written settings. The loader calls it so a typo fails
	// before any resource is built; Build calls it again to get the typed value.
	Decode func(settings json.RawMessage) (any, error)
	// Fingerprint is the non-secret part of the settings. Nil means the whole
	// canonical settings object, which is wrong for anything with a credential.
	Fingerprint func(settings json.RawMessage) string
	Build       func(ctx context.Context, deps EngineDeps) (agent.RunnerFactory, error)
}

// ProtocolKind registers one protocol kind.
type ProtocolKind struct {
	Name string
	// Decode validates the written settings; Build decodes them again.
	Decode func(settings json.RawMessage) (any, error)
	Build  func(ctx context.Context, deps ProtocolDeps) (Protocol, error)
}

// Protocol is an agent's external surface.
type Protocol interface {
	Handler() http.Handler
	Close() error
}

// Kinds is the registered engine and protocol kinds. One value is shared by the
// loader and the manager, so a kind cannot be loadable and yet not deployable.
type Kinds struct {
	Engines   map[string]EngineKind
	Protocols map[string]ProtocolKind
}

// BuiltinKinds returns the kinds every deployment starts with.
func BuiltinKinds() Kinds {
	return Kinds{
		Engines: map[string]EngineKind{
			"a2a":   A2AEngine(),
			"graph": GraphEngine(),
		},
		Protocols: map[string]ProtocolKind{},
	}
}

// WithEngines returns a copy with these engines registered.
func (k Kinds) WithEngines(engines ...EngineKind) Kinds {
	merged := k.clone()
	for _, item := range engines {
		merged.Engines[item.Name] = item
	}
	return merged
}

// WithProtocols returns a copy with these protocols registered.
func (k Kinds) WithProtocols(protocols ...ProtocolKind) Kinds {
	merged := k.clone()
	for _, item := range protocols {
		merged.Protocols[item.Name] = item
	}
	return merged
}

func (k Kinds) clone() Kinds {
	cloned := Kinds{
		Engines:   make(map[string]EngineKind, len(k.Engines)),
		Protocols: make(map[string]ProtocolKind, len(k.Protocols)),
	}
	for name, engine := range k.Engines {
		cloned.Engines[name] = engine
	}
	for name, protocol := range k.Protocols {
		cloned.Protocols[name] = protocol
	}
	return cloned
}

// ValidateEngineSpec applies the rules every kind shares: a local engine has a
// runtime, a remote one has none. Kind-specific fields belong to Decode.
func ValidateEngineSpec(kind EngineKind, spec EngineSpec, hasRuntime bool) error {
	if kind.Local && !hasRuntime {
		return errdefs.Validationf("kind %q needs a runtime", spec.Kind)
	}
	if !kind.Local && hasRuntime {
		return errdefs.Validationf("kind %q has no local runtime to configure", spec.Kind)
	}
	return nil
}

// CanonicalSettings renders settings the same way regardless of the whitespace
// they were written with. Kinds with a credential must provide Fingerprint instead.
func CanonicalSettings(settings json.RawMessage) string {
	if len(settings) == 0 {
		return "{}"
	}
	var value any
	if err := json.Unmarshal(settings, &value); err != nil {
		return string(settings)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return string(settings)
	}
	return string(canonical)
}
