package deploy

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/GizClaw/flowcraft/core/runtime"
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

// LayerDocument is one file in the stack.
type LayerDocument struct {
	Priority int    `json:"priority"`
	Path     string `json:"path"`
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
// error, rather than failing halfway through assembling an agent.
type Loader struct {
	// LoadOptions are handed to runtime.LoadLayers: resolvers for placeholder
	// schemes. They are not Spec fields because they are host wiring, not
	// deployment data.
	LoadOptions []runtime.LoadOption
	// Kinds are the registered engines and protocols. Decode is invoked here, so
	// a bad settings block fails at load time.
	Kinds Kinds
}

// LoadFile reads a document from disk.
func (l Loader) LoadFile(ctx context.Context, path string) ([]Spec, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, errdefs.Validationf("manage: read %s: %w", path, err)
	}
	return l.Load(ctx, body)
}

// RequiredEnv reports the environment variables a deployment document names,
// before the document is loaded. Load fails at the first unset reference; this
// reports all of them in one pass, which is what a host needs to print a startup
// checklist instead of a restart loop.
func (l Loader) RequiredEnv(body []byte) (runtime.EnvRequirement, error) {
	return runtime.RequiredEnvDocument(body, "deployment document")
}

// RequiredEnvFile reads a document and reports its environment requirements.
func (l Loader) RequiredEnvFile(path string) (runtime.EnvRequirement, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return runtime.EnvRequirement{}, errdefs.Validationf("manage: read %s: %w", path, err)
	}
	return runtime.RequiredEnvDocument(body, path)
}

// Load parses and validates a deployment document.
func (l Loader) Load(ctx context.Context, body []byte) ([]Spec, error) {
	var document Document
	// Placeholders are expanded by the same code a runtime layer uses, on the
	// parsed values: peer, auth, card and settings can all name a secret, and a
	// host should not have to learn a second rule for the outer document.
	expanded, err := runtime.ExpandDocument(ctx, body, l.LoadOptions...)
	if err != nil {
		return nil, errdefs.Validationf("manage: %w", err)
	}
	document, err = decode.Decode[Document](expanded)
	if err != nil {
		return nil, errdefs.Validationf("manage: deployment document: %v", err)
	}
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
			where("engine"), agent.Engine.Kind, strings.Join(sortedKeys(l.Kinds.Engines), ", "))
	}

	// The kind says whether it runs here and speaks for its own fields; the
	// shared rules (runtime, local-vs-remote) live beside the registration.
	settings := agent.Engine.Settings
	if len(strings.TrimSpace(string(settings))) == 0 {
		settings = json.RawMessage(`{}`)
	}
	engineSpec := EngineSpec{Kind: agent.Engine.Kind, Settings: settings}
	if kind.Decode != nil {
		if _, err := kind.Decode(settings); err != nil {
			return Spec{}, errdefs.Validationf("%s: %v", where("engine"), err)
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
				where("protocol"), agent.Protocol.Kind, strings.Join(sortedKeys(l.Kinds.Protocols), ", "))
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

	catalog := CatalogSpec{}
	if agent.Catalog != nil {
		catalog.Scope = agent.Catalog.Scope
	}
	if _, err := catalog.ValidatedScope(); err != nil {
		return Spec{}, errdefs.Validationf("%s: %w", where("catalog"), err)
	}
	spec := Spec{
		Name:     agent.Name,
		Catalog:  catalog,
		Engine:   engineSpec,
		Protocol: protocolSpec,
	}
	if agent.Runtime != nil {
		layers, err := l.layers(agent.Name, agent.Runtime)
		if err != nil {
			return Spec{}, err
		}
		config, _, err := runtime.LoadLayers(ctx, layers, l.LoadOptions...)
		if err != nil {
			return Spec{}, errdefs.Validationf("manage: agents[%s]: %w", agent.Name, err)
		}
		spec.Runtime = config
	}
	return spec, nil
}

// layers turns the document's stack into runtime layers.
func (l Loader) layers(agent string, runtimeDocument *RuntimeDocument) ([]runtime.Layer, error) {
	if len(runtimeDocument.Layers) == 0 {
		return nil, errdefs.Validationf(
			"manage: agents[%s].runtime: at least one layer is required", agent)
	}
	layers := make([]runtime.Layer, 0, len(runtimeDocument.Layers))
	for i, layer := range runtimeDocument.Layers {
		if strings.TrimSpace(layer.Path) == "" {
			return nil, errdefs.Validationf(
				"manage: agents[%s].runtime.layers[%d]: path is required", agent, i)
		}
		layers = append(layers, runtime.Layer{
			Priority: layer.Priority,
			Name:     layer.Path,
			Path:     layer.Path,
		})
	}
	return layers, nil
}

func (l Loader) engine(name string) (EngineKind, bool) {
	kind, found := l.Kinds.Engines[name]
	return kind, found
}

func (l Loader) protocol(name string) (ProtocolKind, bool) {
	kind, found := l.Kinds.Protocols[name]
	return kind, found
}
