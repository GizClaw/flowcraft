package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/goccy/go-json"
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
	// Trace is how much of a run this engine traces: its spans, and at the node
	// tier the per-call evidence it keeps.
	Trace telemetry.TraceLevel
	// Bodies keeps the bytes of recorded model responses, which is what a replay
	// answers from.
	Bodies bool
	// Sources opens the documents this engine's settings name, from the same table
	// a runtime layer is read from.
	Sources SourceOpener
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

// SourceOpener opens a document a deployment names by source kind. It is the
// loader's table bound to one location, so a kind that reads a document of its
// own reports a bad source the same way a runtime layer does.
type SourceOpener func(ctx context.Context, document *SourceDocument) (Source, error)

// EngineKind registers one engine kind.
type EngineKind struct {
	Name string
	// Local marks an engine that runs in this process: it needs a runtime and
	// has no peer. A remote engine is the opposite, and the loader validates each
	// kind by what it says here instead of by its name.
	Local bool
	// Decode validates the written settings. The loader calls it so a typo fails
	// before any resource is built; Build calls it again to get the typed value.
	//
	// An engine is the only kind whose settings may name a document the loader
	// has to read before it can validate anything, which is why Decode and
	// Fingerprint receive the source table and a protocol's or a hook's plain
	// Decode does not.
	Decode func(ctx context.Context, settings json.RawMessage, sources SourceOpener) (any, error)
	// Fingerprint is the non-secret part of the settings. Nil means the whole
	// canonical settings object, which is wrong for anything with a credential.
	// It receives the source table for the same reason Decode does: a document an
	// engine is driven by is part of its identity, bytes included.
	Fingerprint func(ctx context.Context, settings json.RawMessage, sources SourceOpener) string
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

// HookSpec selects one host hook kind and carries its settings.
type HookSpec struct {
	// Kind is required. A hook a deployment names but the host did not register
	// is a document error, not a silently absent policy.
	Kind string
	// Settings is the kind's own configuration as written (JSON).
	Settings json.RawMessage
}

// HookDeps is what a hook kind is built from.
type HookDeps struct {
	// Agent is the deployment name the hook is attached to: what a seat-bound
	// policy keys itself by.
	Agent string
	// Spec is the kind and its settings.
	Spec HookSpec
	// Runtime is the agent's substrate, nil for a remote engine: a hook that only
	// observes turns does not need one.
	Runtime *runtime.Runtime
	// Store is the deployment's checkpoint store, so a hook can read the task log
	// to assemble what it needs.
	Store agent.Store
	// LoadOptions resolve placeholders for a kind that reads its own documents.
	LoadOptions []config.Option
}

// HookKind registers one host hook kind: a policy attached to an agent's lifecycle.
type HookKind struct {
	Name string
	// Decode validates the written settings. The loader calls it so a typo fails
	// before the agent is built; Build calls it again to get the typed value.
	Decode func(settings json.RawMessage) (any, error)
	// Build composes this hook. Several hooks on one agent are chained in
	// declaration order.
	Build func(ctx context.Context, deps HookDeps) (agent.Hooks, error)
}

// Source is one document read through a source kind: its bytes and the name to
// report them under. It is the deploy-side counterpart of [config.Source], which
// is only bytes because a merge has no reader to name them for.
type Source struct {
	// Name is what errors report: the layer a merge failed on, the document the
	// graph engine could not parse. It is not a path — an embedded document has
	// none — and it is what the graph deployment's identity is built from.
	Name string
	// Body is where the bytes come from. [config.File], [config.Bytes],
	// [config.FS] and [config.Func] are the four ways core supplies one; a host
	// reading a secret store or an embedded tree writes its own.
	Body config.Source
}

// Read returns the layer's bytes. A kind that reads a document of its own calls
// it; the merge reads through Body directly, because it wraps the error with the
// provenance of the load rather than of one layer.
func (s Source) Read(ctx context.Context) ([]byte, error) {
	if s.Body == nil {
		return nil, errdefs.Validationf("%q has no body", s.Name)
	}
	return s.Body.Body(ctx)
}

// SourceKind registers one way a document's bytes are obtained: a file on this
// host, a member of an //go:embed tree, a key in a secret store.
//
// Open decodes its own settings because it runs at load time, while the layers
// are merged. That is also why there is no separate Decode as there is for
// engines: a source is opened once and has nothing left to build.
type SourceKind struct {
	// Name is what a document writes in source.kind: a runtime layer, an engine's
	// settings, or the deployment document itself.
	Name string
	// Open produces the document from the written settings. The context is the
	// load's, so a source that reaches a network or a vault can be cancelled.
	//
	// The same reference is opened more than once for one deployment — to
	// validate, for the engine's identity, and to compile — so a source has to
	// return the same bytes each time within one load. A source that cannot
	// (a token that rotates per call, a document generated afresh) makes the
	// recorded identity advisory rather than the identity of what ran.
	Open func(ctx context.Context, settings json.RawMessage) (Source, error)
}

// Kinds is the registered engine, protocol, hook and source kinds. One value is
// shared by the loader and the manager, so a kind cannot be loadable and yet not
// deployable.
//
// This package registers nothing: a registration belongs to the package that
// knows the implementation (deploy/engines/graph, deploy/engines/a2a,
// deploy/protocols/a2a, deploy/builtin), so a host that runs one engine links one
// engine. Start from the zero value and add what the deployment names — engines,
// and the sources its documents are read through:
//
//	kinds := deploy.Kinds{}.
//		WithEngines(enginesgraph.Engine()).
//		WithSources(sources.File())
//
// deploy/builtin carries the whole set in one call, for a host that wants every
// kind core ships.
type Kinds struct {
	Engines   map[string]EngineKind
	Protocols map[string]ProtocolKind
	Hooks     map[string]HookKind
	// Sources are the ways a document's bytes are obtained. The zero table
	// registers none, so a document naming a source fails to load;
	// deploy/builtin registers file and inline.
	Sources map[string]SourceKind
}

// OpenSource opens the source a document names, naming at in every error it
// returns.
//
// There is one resolution rule: the loader uses it for a runtime layer, and a
// kind driven by a document of its own — the graph engine is one — uses it
// through [SourceOpener]. A layer and an engine therefore cannot disagree about
// what a source kind means.
func (k Kinds) OpenSource(ctx context.Context, at string, document *SourceDocument) (Source, error) {
	opened, err := k.openSource(ctx, document)
	if err != nil {
		return Source{}, errdefs.Validationf("%s: %w", at, err)
	}
	return opened, nil
}

// Opener returns this table as a [SourceOpener] for a caller that names the
// location itself: a source's error is prefixed with label when it is not empty
// and left bare when it is, so the caller adds its own location exactly once.
func (k Kinds) Opener(label string) SourceOpener {
	return func(ctx context.Context, document *SourceDocument) (Source, error) {
		opened, err := k.openSource(ctx, document)
		if err != nil && label != "" {
			return Source{}, errdefs.Validationf("%s: %w", label, err)
		}
		return opened, err
	}
}

// openSource resolves a document reference without a location, so its caller can
// name the place exactly once: an agent's engine settings, a stack entry, or the
// deployment document itself.
func (k Kinds) openSource(ctx context.Context, document *SourceDocument) (Source, error) {
	if document == nil {
		return Source{}, errdefs.Validationf(
			"source is required; write source: {kind: file, settings: {path: ...}}")
	}
	if strings.TrimSpace(document.Path) != "" {
		return Source{}, errdefs.Validationf(
			"source.path was replaced by source.settings.path; write source: {kind: file, settings: {path: %q}}",
			document.Path)
	}
	if strings.TrimSpace(document.Kind) == "" {
		return Source{}, errdefs.Validationf("source.kind is required")
	}
	kind, found := k.Sources[document.Kind]
	if !found {
		return Source{}, errdefs.Validationf(
			"source: kind %q is not registered; offered: %s",
			document.Kind, offered(k.Sources, "deploy/builtin registers file and inline"))
	}
	if kind.Open == nil {
		return Source{}, errdefs.Validationf(
			"source: kind %q is registered without Open", document.Kind)
	}
	settings := document.Settings
	if len(strings.TrimSpace(string(settings))) == 0 {
		settings = json.RawMessage(`{}`)
	}
	opened, err := kind.Open(ctx, settings)
	if err != nil {
		return Source{}, errdefs.Validationf("source: %w", err)
	}
	// A kind that produced nothing would merge as a silent no-op, and a layer
	// nobody can name is a layer nobody can fix: both are registration bugs, and
	// both are reported where the document named them.
	if opened.Body == nil {
		return Source{}, errdefs.Validationf(
			"source: kind %q produced no body", document.Kind)
	}
	if strings.TrimSpace(opened.Name) == "" {
		return Source{}, errdefs.Validationf(
			"source: kind %q produced no name", document.Kind)
	}
	return opened, nil
}

// WithEngines returns a copy with these engines registered.
func (k Kinds) WithEngines(engines ...EngineKind) Kinds {
	merged := k.clone()
	for _, item := range engines {
		merged.Engines[item.Name] = item
	}
	return merged
}

// WithHooks returns a copy with these hook kinds registered.
func (k Kinds) WithHooks(hooks ...HookKind) Kinds {
	merged := k.clone()
	for _, item := range hooks {
		merged.Hooks[item.Name] = item
	}
	return merged
}

// WithSources returns a copy with these layer sources registered.
func (k Kinds) WithSources(sources ...SourceKind) Kinds {
	merged := k.clone()
	for _, item := range sources {
		merged.Sources[item.Name] = item
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
		Hooks:     make(map[string]HookKind, len(k.Hooks)),
		Sources:   make(map[string]SourceKind, len(k.Sources)),
	}
	for name, engine := range k.Engines {
		cloned.Engines[name] = engine
	}
	for name, protocol := range k.Protocols {
		cloned.Protocols[name] = protocol
	}
	for name, hook := range k.Hooks {
		cloned.Hooks[name] = hook
	}
	for name, source := range k.Sources {
		cloned.Sources[name] = source
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

// InvalidIdentity is the identity of settings that cannot be resolved to a
// document at all.
//
// Decode refuses such settings, so this is unreachable through the loader. If a
// host builds a Spec by hand and reaches it anyway, the identity must differ from
// every real deployment's — and from every other broken one's, because two
// deployments sharing an identity would resume each other's checkpoints.
func InvalidIdentity(settings json.RawMessage) string {
	sum := sha256.Sum256(settings)
	return "invalid:" + hex.EncodeToString(sum[:])
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
