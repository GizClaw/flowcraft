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
}

// EngineDocument selects the implementation and carries its settings.
type EngineDocument struct {
	// Kind is required. There is no default implementation: an agent that does
	// not say what it is cannot be deployed meaningfully.
	Kind string `json:"kind"`
	// Peer is the remote card URL for the a2a engine.
	Peer string `json:"peer"`
	// Auth is the credential for reaching Peer.
	Auth json.RawMessage `json:"auth"`
	// Graph is the graph document path for the graph engine.
	Graph string `json:"graph"`
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
	// Card is the a2a protocol's advertisement, written beside kind.
	Card json.RawMessage `json:"card"`
	// Settings is the protocol's own configuration, decoded by its factory.
	Settings json.RawMessage `json:"settings"`
}

// Loader turns a deployment document into specs.
//
// The kind tables are names, not factories yet: an engine that is not registered
// yet must still be rejected at load time, with the registered names in the
// error, rather than failing halfway through assembling an agent.
type Loader struct {
	// LoadOptions are handed to runtime.LoadLayers: resolvers for placeholder
	// schemes. They are not Spec fields because they are host wiring, not
	// deployment data.
	LoadOptions []runtime.LoadOption
	// Engines and Protocols are the registered kinds. Decode is invoked here so
	// a bad protocol block fails at load time.
	Engines   []Engine
	Protocols []Protocol
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
	if _, found := l.engine(agent.Engine.Kind); !found {
		return Spec{}, errdefs.Validationf(
			"%s: kind %q is not registered; offered: %s",
			where("engine"), agent.Engine.Kind, strings.Join(engineNames(l.Engines), ", "))
	}

	// The engine decides whether a substrate is needed: a proxy has no local
	// model, tools or sandbox, and an engine that runs locally cannot forward to
	// a peer.
	needsRuntime := agent.Engine.Kind != "a2a"
	switch {
	case needsRuntime && agent.Runtime == nil:
		return Spec{}, errdefs.Validationf(
			"%s: kind %q needs a runtime", where("engine"), agent.Engine.Kind)
	case !needsRuntime && agent.Runtime != nil:
		return Spec{}, errdefs.Validationf(
			"%s: kind %q has no local runtime to configure", where("engine"), agent.Engine.Kind)
	}
	switch agent.Engine.Kind {
	case "a2a":
		if strings.TrimSpace(agent.Engine.Peer) == "" {
			return Spec{}, errdefs.Validationf("%s: peer is required", where("engine"))
		}
	default:
		if agent.Engine.Peer != "" {
			return Spec{}, errdefs.Validationf(
				"%s: peer belongs to the a2a engine", where("engine"))
		}
	}

	var protocolSpec *ProtocolSpec
	var decoded any
	if agent.Protocol != nil {
		if strings.TrimSpace(agent.Protocol.Kind) == "" {
			return Spec{}, errdefs.Validationf("%s: kind is required", where("protocol"))
		}
		registration, found := l.protocol(agent.Protocol.Kind)
		if !found {
			return Spec{}, errdefs.Validationf(
				"%s: kind %q is not registered; offered: %s",
				where("protocol"), agent.Protocol.Kind, strings.Join(protocolNames(l.Protocols), ", "))
		}
		settings := agent.Protocol.Settings
		if len(agent.Protocol.Card) > 0 {
			// The written form puts the card beside kind; the decoder reads it
			// from the settings object, so fold it in without mutating the
			// document.
			fields := make(map[string]json.RawMessage, 1)
			if len(settings) > 0 {
				if err := json.Unmarshal(settings, &fields); err != nil {
					return Spec{}, errdefs.Validationf("%s: settings: %w", where("protocol"), err)
				}
			}
			fields["card"] = agent.Protocol.Card
			merged, err := json.Marshal(fields)
			if err != nil {
				return Spec{}, errdefs.Validationf("%s: settings: %w", where("protocol"), err)
			}
			settings = merged
		}
		protocolSpec = &ProtocolSpec{Kind: agent.Protocol.Kind, Settings: settings}
		if registration.Decode != nil {
			value, err := registration.Decode(settings)
			if err != nil {
				return Spec{}, errdefs.Validationf("%s: %w", where("protocol"), err)
			}
			decoded = value
		}
	}

	spec := Spec{
		Name: agent.Name,
		Engine: EngineSpec{
			Kind:  agent.Engine.Kind,
			Peer:  agent.Engine.Peer,
			Auth:  agent.Engine.Auth,
			Graph: agent.Engine.Graph,
		},
		Protocol: protocolSpec,
	}
	if protocolSpec != nil {
		protocolSpec.Decoded = decoded
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

func (l Loader) engine(name string) (Engine, bool) {
	for _, candidate := range l.Engines {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return Engine{}, false
}

func (l Loader) protocol(name string) (Protocol, bool) {
	for _, candidate := range l.Protocols {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return Protocol{}, false
}

func engineNames(engines []Engine) []string {
	names := make([]string, 0, len(engines))
	for _, engine := range engines {
		names = append(names, engine.Name)
	}
	return names
}

func protocolNames(protocols []Protocol) []string {
	names := make([]string, 0, len(protocols))
	for _, item := range protocols {
		names = append(names, item.Name)
	}
	return names
}
