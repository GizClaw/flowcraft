package deploy

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/goccy/go-json"
)

// Document is the deployment file: which agents exist and what each is made of.
type Document struct {
	Version int             `json:"version"`
	Agents  []AgentDocument `json:"agents"`
}

// AgentDocument is one agent's declaration.
type AgentDocument struct {
	Name     string            `json:"name"`
	Engine   EngineDocument    `json:"engine"`
	Runtime  *RuntimeDocument  `json:"runtime"`
	Protocol *ProtocolDocument `json:"protocol"`
	// Catalog scopes the tool projection a local engine reads.
	Catalog *CatalogDocument `json:"catalog"`
	// Delegation is the policy this agent's delegate tools use. Nil means the
	// agent gets no delegation tools at all.
	Delegation *DelegationDocument `json:"delegation"`
	// Observability is how much of this agent's runs is traced and recorded.
	// Nil keeps the host's defaults for both.
	Observability *ObservabilityDocument `json:"observability"`
	// Hooks are the host policies attached to this agent's lifecycle, in
	// declaration order.
	Hooks []HookDocument `json:"hooks"`
}

// ObservabilityDocument is the written form of the run's observation policy.
//
// Trace is one ladder because spans and the per-call records answer the same
// question at different resolutions: a run somebody wants to inspect node by
// node is also a run whose prompt digests are worth keeping. Bodies is not a
// level: whether the response's bytes are stored is a retention decision about
// the user's own content, so it is asked separately.
type ObservabilityDocument struct {
	// Trace is off (default), run, step or node.
	Trace string `json:"trace"`
	// Bodies keeps the bytes of recorded model responses. Nil means the host's
	// default.
	Bodies *bool `json:"bodies"`
}

// HookDocument selects one host hook kind and carries its settings.
type HookDocument struct {
	// Kind is required when the entry is present. An unregistered kind is a
	// document error naming what is registered, the same rule engines and
	// protocols follow.
	Kind string `json:"kind"`
	// Settings is the kind's own configuration, decoded by its registration.
	Settings json.RawMessage `json:"settings"`
}

// DelegationDocument is the written form of a delegation policy.
type DelegationDocument struct {
	Targets       []string `json:"targets,omitempty"`
	Modes         []string `json:"modes,omitempty"`
	MaxDepth      int      `json:"max_depth,omitempty"`
	MaxConcurrent int      `json:"max_concurrent,omitempty"`
	Timeout       string   `json:"timeout,omitempty"`
	MaxInputBytes int      `json:"max_input_bytes,omitempty"`
}

// CatalogDocument scopes the tool projection.
type CatalogDocument struct {
	// Scope is "run" (default) or "session".
	Scope string `json:"scope"`
}

// EngineDocument selects the implementation and carries its settings.
type EngineDocument struct {
	// Kind is required. There is no default implementation: an agent that does
	// not say what it is cannot be deployed meaningfully.
	Kind string `json:"kind"`
	// Settings is the kind's own configuration, decoded by its registration.
	Settings json.RawMessage `json:"settings"`
}

// RuntimeDocument is the substrate configuration as a stack of layers.
type RuntimeDocument struct {
	Layers []LayerDocument `json:"layers"`
}

// LayerDocument is one layer in the stack.
type LayerDocument struct {
	// Priority orders the layer; higher wins.
	Priority int `json:"priority"`
	// Source is how this layer's bytes are obtained. It is required: a layer
	// whose bytes come from nowhere cannot be merged, and defaulting to a file
	// would be a choice the document made by accident.
	Source *SourceDocument `json:"source"`
	// Path is the field Source replaced. It is decoded only so an old document is
	// told what to write instead of failing as an unknown field, which names
	// neither the location nor the replacement.
	Path string `json:"path"`
}

// SourceDocument selects one registered source and carries its settings. It is
// the same block wherever a document names bytes: a runtime layer, an engine's
// own document, or the deployment document itself.
type SourceDocument struct {
	// Kind is required. An unregistered kind is a document error naming what is
	// registered, the same rule engines, protocols and hooks follow.
	Kind string `json:"kind"`
	// Settings is the kind's own configuration, decoded by its registration.
	Settings json.RawMessage `json:"settings"`
	// Path is the half-migrated spelling this block refuses: a document that
	// copied an old path next to kind instead of inside settings. It is decoded
	// only so the reader is told where the field went.
	Path string `json:"path"`
}

// ProtocolDocument selects the exposure and carries its settings.
type ProtocolDocument struct {
	// Kind is required when the block is present.
	Kind string `json:"kind"`
	// Settings is the protocol's own configuration, decoded by its kind.
	Settings json.RawMessage `json:"settings"`
}

// Loader turns a deployment document into specs.
//
// The same kind table the manager uses decides what a document may name: an
// unregistered kind is rejected at load time, with the registered names in the
// error, rather than failing halfway through assembling an agent. That covers
// the engines and protocols an agent is made of and the sources its runtime
// layers are read from.
type Loader struct {
	// LoadOptions are handed to [config.Load]: resolvers for placeholder
	// schemes. They are not Spec fields because they are host wiring, not
	// deployment data.
	LoadOptions []config.Option
	// Kinds are the registered engines, protocols, hooks and layer sources.
	// Decode and Open are invoked here, so a bad settings block fails at load
	// time.
	Kinds Kinds
}

// FileSourceDocument is the file source written as a document reference: the same
// block a deployment writes in runtime.layers[].source. It exists so that
// [Loader.LoadFile] is sugar over [Loader.LoadSource] rather than a second rule
// about where a document may live.
func FileSourceDocument(path string) *SourceDocument {
	settings, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		// A map of strings cannot fail to marshal. If it ever does, an empty
		// settings block is refused by the file source instead of being guessed
		// at here.
		return &SourceDocument{Kind: "file"}
	}
	return &SourceDocument{Kind: "file", Settings: settings}
}

// LoadSource reads a deployment document through a source kind: the same table a
// runtime layer and a graph document are read from.
//
// It is the entry for a host whose document is embedded, fetched or stored: it
// names the source once instead of holding the bytes itself. [Loader.Load] is the
// entry for a body that is already in memory, and [Loader.LoadFile] is this with
// the file source.
func (l Loader) LoadSource(ctx context.Context, source *SourceDocument) ([]Spec, error) {
	_, body, err := l.documentBody(ctx, source)
	if err != nil {
		return nil, err
	}
	return l.Load(ctx, body)
}

// LoadFile reads a document from disk: [Loader.LoadSource] with the file source,
// so the kind table must register it (deploy/builtin does).
func (l Loader) LoadFile(ctx context.Context, path string) ([]Spec, error) {
	return l.LoadSource(ctx, FileSourceDocument(path))
}

// documentBody opens the deployment document itself. The location is not a field
// path: this is the root, and every error below it names the agent it belongs to.
func (l Loader) documentBody(ctx context.Context, source *SourceDocument) (string, []byte, error) {
	const at = "manage: deployment document"
	opened, err := l.Kinds.OpenSource(ctx, at, source)
	if err != nil {
		return "", nil, err
	}
	body, err := opened.Read(ctx)
	if err != nil {
		return "", nil, errdefs.Validationf("%s: %s: %w", at, opened.Name, err)
	}
	return opened.Name, body, nil
}

// RequiredPlaceholders reports the environment variables a deployment document names,
// before the document is loaded. Load fails at the first unset reference; this
// reports all of them in one pass, which is what a host needs to print a startup
// checklist instead of a restart loop.
func (l Loader) RequiredPlaceholders(body []byte) (config.Requirement, error) {
	return config.RequiredPlaceholdersDocument(body, "deployment document")
}

// RequiredPlaceholdersSource does the same for a document read through a source
// kind.
func (l Loader) RequiredPlaceholdersSource(ctx context.Context, source *SourceDocument) (config.Requirement, error) {
	name, body, err := l.documentBody(ctx, source)
	if err != nil {
		return config.Requirement{}, err
	}
	return config.RequiredPlaceholdersDocument(body, name)
}

// RequiredPlaceholdersFile reads a document from disk and reports its environment
// requirements. Like [Loader.LoadFile], it needs the file source registered.
func (l Loader) RequiredPlaceholdersFile(ctx context.Context, path string) (config.Requirement, error) {
	return l.RequiredPlaceholdersSource(ctx, FileSourceDocument(path))
}

// Load parses and validates a deployment document.
func (l Loader) Load(ctx context.Context, body []byte) ([]Spec, error) {
	document, err := config.Decode[Document](ctx, body, config.Strict[Document], l.settings()...)
	if err != nil {
		return nil, err
	}
	return l.specs(ctx, document)
}

// LoadLayers parses and validates a stack of deployment documents.
//
// It is [Loader.Load] over a merged stack rather than one body: the layers merge
// in ascending priority, placeholders are expanded once after the merge, and the
// merged document is validated exactly as a single one is. It lets a host ship
// defaults that a project and then a user refine, mirroring the shape
// [runtime.LoadLayers] gives a runtime configuration.
//
// The merge is core/config's, so its rules are the document's rules: a mapping
// merges key by key, a sequence is replaced whole. `agents` is a sequence, so a
// higher layer replaces the agent list instead of patching one entry's fields: an
// agent a later layer wants to change has to be written out in full there, and the
// same holds for an agent's own `runtime.layers`.
//
// [Loader.LoadOptions] carries the resolvers, which is why this entry takes no
// options of its own: a stack and a body must not be able to disagree about what
// a placeholder scheme means.
func (l Loader) LoadLayers(ctx context.Context, layers []config.Layer) ([]Spec, error) {
	document, _, err := config.Load[Document](ctx, layers, config.Strict[Document], l.settings()...)
	if err != nil {
		return nil, err
	}
	return l.specs(ctx, document)
}

// settings are the options both entries hand to core/config: the host's
// resolvers, then the label every error below this point is prefixed with.
//
// Placeholders are expanded by the same code a configuration layer uses, on the
// parsed values: peer, auth, card and settings can all name a secret, and a host
// should not have to learn a second rule for the outer document.
func (l Loader) settings() []config.Option {
	settings := append([]config.Option{}, l.LoadOptions...)
	return append(settings, config.WithLabel("manage: deployment document"))
}

// specs validates a document and builds one spec per agent. It is the half the
// two entries share, so a layered load cannot be looser than a single-body one.
func (l Loader) specs(ctx context.Context, document Document) ([]Spec, error) {
	if document.Version != 1 {
		return nil, errdefs.Validationf(
			"manage: version %d is not supported; this build reads 1", document.Version)
	}
	if len(document.Agents) == 0 {
		return nil, errdefs.Validationf("manage: the document declares no agents")
	}
	seen := make(map[string]struct{}, len(document.Agents))
	specs := make([]Spec, 0, len(document.Agents))
	for i, agent := range document.Agents {
		spec, err := l.agent(ctx, i, agent)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[agent.Name]; duplicate {
			return nil, errdefs.Validationf(
				"manage: agents[%d]: duplicate name %q", i, agent.Name)
		}
		seen[agent.Name] = struct{}{}
		specs = append(specs, spec)
	}
	return specs, nil
}

// agent validates one entry and builds its spec.
func (l Loader) agent(ctx context.Context, index int, agent AgentDocument) (Spec, error) {
	where := func(field string) string {
		return "manage: agents[" + agent.Name + "]." + field
	}
	if strings.TrimSpace(agent.Name) == "" {
		return Spec{}, errdefs.Validationf("manage: agents[%d]: name is required", index)
	}
	if strings.TrimSpace(agent.Engine.Kind) == "" {
		return Spec{}, errdefs.Validationf("%s: kind is required", where("engine"))
	}
	kind, found := l.engine(agent.Engine.Kind)
	if !found {
		return Spec{}, errdefs.Validationf(
			"%s: kind %q is not registered; offered: %s",
			where("engine"), agent.Engine.Kind,
			Offered(l.Kinds.Engines, "deploy/builtin registers graph and a2a"))
	}

	// The kind says whether it runs here and speaks for its own fields; the
	// shared rules (runtime, local-vs-remote) live beside the registration.
	settings := agent.Engine.Settings
	if len(strings.TrimSpace(string(settings))) == 0 {
		settings = json.RawMessage(`{}`)
	}
	engineSpec := EngineSpec{Kind: agent.Engine.Kind, Settings: settings}
	if kind.Decode != nil {
		// The opener carries no location: a kind names what its settings are
		// about, a source names what it could not open, and the agent is named
		// here — once, rather than as a prefix over a message that already
		// names the same field.
		if _, err := kind.Decode(ctx, settings, l.opener()); err != nil {
			return Spec{}, errdefs.Validationf("%s: %w", where("engine"), err)
		}
	}
	if err := ValidateEngineSpec(kind, engineSpec, agent.Runtime != nil); err != nil {
		return Spec{}, errdefs.Validationf("%s: %v", where("engine"), err)
	}

	var protocolSpec *ProtocolSpec
	if agent.Protocol != nil {
		if strings.TrimSpace(agent.Protocol.Kind) == "" {
			return Spec{}, errdefs.Validationf("%s: kind is required", where("protocol"))
		}
		kind, found := l.protocol(agent.Protocol.Kind)
		if !found {
			return Spec{}, errdefs.Validationf(
				"%s: kind %q is not registered; offered: %s",
				where("protocol"), agent.Protocol.Kind,
				Offered(l.Kinds.Protocols, "deploy/builtin registers a2a"))
		}
		settings := agent.Protocol.Settings
		if len(strings.TrimSpace(string(settings))) == 0 {
			settings = json.RawMessage(`{}`)
		}
		if kind.Decode != nil {
			if _, err := kind.Decode(settings); err != nil {
				return Spec{}, errdefs.Validationf("%s: %v", where("protocol"), err)
			}
		}
		protocolSpec = &ProtocolSpec{Kind: agent.Protocol.Kind, Settings: settings}
	}

	hooks, err := l.hookSpecs(agent.Name, agent.Hooks, where)
	if err != nil {
		return Spec{}, err
	}

	catalog := CatalogSpec{}
	if agent.Catalog != nil {
		catalog.Scope = agent.Catalog.Scope
	}
	if _, err := catalog.ValidatedScope(); err != nil {
		return Spec{}, errdefs.Validationf("%s: %w", where("catalog"), err)
	}
	delegationSpec, err := buildDelegation(agent.Delegation, where)
	if err != nil {
		return Spec{}, err
	}
	observability, err := buildObservability(agent.Observability, where)
	if err != nil {
		return Spec{}, err
	}
	spec := Spec{
		Name:          agent.Name,
		Catalog:       catalog,
		Hooks:         hooks,
		Engine:        engineSpec,
		Protocol:      protocolSpec,
		Delegation:    delegationSpec,
		Observability: observability,
	}
	if agent.Runtime != nil {
		layers, err := l.layers(ctx, agent.Name, agent.Runtime)
		if err != nil {
			return Spec{}, err
		}
		built, err := runtime.LoadLayers(ctx, layers, l.LoadOptions...)
		if err != nil {
			return Spec{}, errdefs.Validationf("manage: agents[%s]: %w", agent.Name, err)
		}
		spec.Runtime = built
	}
	return spec, nil
}

// layers turns the document's stack into configuration layers.
//
// Each layer's bytes come from the source kind it names, so the document states
// where a layer lives without deciding what "where" means: a file on this host is
// one registration, an embedded tree or a secret store is another, and the loader
// tells them apart only by the kind.
func (l Loader) layers(ctx context.Context, agent string, runtimeDocument *RuntimeDocument) ([]config.Layer, error) {
	if len(runtimeDocument.Layers) == 0 {
		return nil, errdefs.Validationf(
			"manage: agents[%s].runtime: at least one layer is required", agent)
	}
	layers := make([]config.Layer, 0, len(runtimeDocument.Layers))
	seen := make(map[string]struct{}, len(runtimeDocument.Layers))
	for i, layer := range runtimeDocument.Layers {
		at := "manage: agents[" + agent + "].runtime.layers[" + strconv.Itoa(i) + "]"
		if strings.TrimSpace(layer.Path) != "" {
			return nil, errdefs.Validationf(
				"%s: path was replaced by source; write source: {kind: file, settings: {path: %q}}",
				at, layer.Path)
		}
		source, err := l.Kinds.OpenSource(ctx, at, layer.Source)
		if err != nil {
			return nil, err
		}
		// Layer names are how a merge failure points at its layer, so two
		// anonymous inline layers in one stack would be indistinguishable. A file
		// source names itself by the path it reads, and the kind has no `name`
		// setting to obey a "name one of them" remedy, so listing one file twice
		// (redundant, not ambiguous) stays legal.
		if layer.Source != nil && layer.Source.Kind != "file" {
			if _, duplicate := seen[source.Name]; duplicate {
				return nil, errdefs.Validationf(
					"%s: two layers report the name %q; name one of them", at, source.Name)
			}
			seen[source.Name] = struct{}{}
		}
		layers = append(layers, config.Layer{
			Priority: layer.Priority,
			// The source names the layer: it is the only part that knows whether
			// this stack has paths or embedded members in it.
			Name:   source.Name,
			Source: source.Body,
		})
	}
	return layers, nil
}

// opener hands a kind the deployment's source table without a location: the
// loader names the agent around whatever the kind or its source reports.
func (l Loader) opener() SourceOpener {
	return l.Kinds.Opener("")
}

func (l Loader) engine(name string) (EngineKind, bool) {
	kind, found := l.Kinds.Engines[name]
	return kind, found
}

func (l Loader) protocol(name string) (ProtocolKind, bool) {
	kind, found := l.Kinds.Protocols[name]
	return kind, found
}

// buildObservability validates the written level. It is checked here rather
// than at build time so a typo fails before an agent exists, and the error names
// what may be written instead — the reader's question is "which values are
// there", not "which field is wrong".
func buildObservability(doc *ObservabilityDocument, where func(string) string) (ObservabilitySpec, error) {
	if doc == nil {
		return ObservabilitySpec{}, nil
	}
	if _, err := telemetry.ParseTraceLevel(doc.Trace); err != nil {
		return ObservabilitySpec{}, errdefs.Validationf("%s: %v", where("observability"), err)
	}
	return ObservabilitySpec{Trace: doc.Trace, Bodies: doc.Bodies}, nil
}

// buildDelegation validates and converts the written policy.
func buildDelegation(doc *DelegationDocument, where func(string) string) (*DelegationSpec, error) {
	if doc == nil {
		return nil, nil
	}
	spec := &DelegationSpec{
		Targets:       append([]string(nil), doc.Targets...),
		Modes:         append([]string(nil), doc.Modes...),
		MaxDepth:      doc.MaxDepth,
		MaxConcurrent: doc.MaxConcurrent,
		MaxInputBytes: doc.MaxInputBytes,
	}
	for _, mode := range spec.Modes {
		if mode != "sync" && mode != "async" {
			return nil, errdefs.Validationf("%s: unknown mode %q", where("delegation"), mode)
		}
	}
	if spec.MaxDepth < 0 {
		return nil, errdefs.Validationf("%s: max_depth must not be negative", where("delegation"))
	}
	if spec.MaxConcurrent < 0 {
		return nil, errdefs.Validationf("%s: max_concurrent must not be negative", where("delegation"))
	}
	if spec.MaxInputBytes < 0 {
		return nil, errdefs.Validationf("%s: max_input_bytes must not be negative", where("delegation"))
	}
	if doc.Timeout != "" {
		timeout, err := time.ParseDuration(doc.Timeout)
		if err != nil {
			return nil, errdefs.Validationf("%s: timeout: %v", where("delegation"), err)
		}
		if timeout < 0 {
			return nil, errdefs.Validationf("%s: timeout must not be negative", where("delegation"))
		}
		spec.Timeout = timeout
	}
	return spec, nil
}

// hookSpecs validates the document's hook list against the registered kinds.
//
// A kind is resolved here rather than at build, so a deployment that names a hook
// nothing implements fails before an agent exists - and the error lists what is
// registered, because that is the question the reader has.
func (l Loader) hookSpecs(agent string, documents []HookDocument, where func(string) string) ([]HookSpec, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	specs := make([]HookSpec, 0, len(documents))
	for i, document := range documents {
		at := where("hooks[" + strconv.Itoa(i) + "]")
		if strings.TrimSpace(document.Kind) == "" {
			return nil, errdefs.Validationf("%s: kind is required", at)
		}
		kind, found := l.Kinds.Hooks[document.Kind]
		if !found {
			return nil, errdefs.Validationf("%s: kind %q is not registered; offered: %s",
				at, document.Kind,
				Offered(l.Kinds.Hooks, "core ships none: a hook is the host's own policy"))
		}
		settings := document.Settings
		if len(strings.TrimSpace(string(settings))) == 0 {
			settings = json.RawMessage(`{}`)
		}
		if kind.Decode != nil {
			if _, err := kind.Decode(settings); err != nil {
				return nil, errdefs.Validationf("%s: %v", at, err)
			}
		}
		specs = append(specs, HookSpec{Kind: document.Kind, Settings: settings})
	}
	return specs, nil
}
