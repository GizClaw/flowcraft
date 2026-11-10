package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/GizClaw/flowcraft/core/runtime"
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
	Spec    EngineSpec
	Runtime *runtime.Runtime
	Store   agent.Store
	Digest  runtime.Digest
}

// ProtocolDeps is what a protocol kind is built from. It carries no deployment
// internals: name, store, identity and the session table are all it needs.
type ProtocolDeps struct {
	Spec        ProtocolSpec
	Name        string
	Store       agent.Store
	Digest      runtime.Digest
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
	// Digest is the non-secret part of the settings. Nil means the whole
	// canonical settings object, which is wrong for anything with a credential.
	Digest func(settings json.RawMessage) string
	Build  func(ctx context.Context, deps EngineDeps) (agent.RunnerFactory, error)
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
			"a2a": {
				Name:   "a2a",
				Decode: decodeA2ASettings,
				Digest: a2aDigest,
				Build:  buildA2AEngine,
			},
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

// validateEngineSpec applies the rules every kind shares: a local engine has a
// runtime, a remote one has none. Kind-specific fields belong to Decode.
func validateEngineSpec(kind EngineKind, spec EngineSpec, hasRuntime bool) error {
	if kind.Local && !hasRuntime {
		return errdefs.Validationf("kind %q needs a runtime", spec.Kind)
	}
	if !kind.Local && hasRuntime {
		return errdefs.Validationf("kind %q has no local runtime to configure", spec.Kind)
	}
	return nil
}

// canonicalSettings renders settings the same way regardless of the whitespace
// they were written with. Kinds with a credential must provide Digest instead.
func canonicalSettings(settings json.RawMessage) string {
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

// a2aSettings is the a2a engine's written configuration.
type a2aSettings struct {
	Peer string          `json:"peer"`
	Auth json.RawMessage `json:"auth"`
}

func decodeA2ASettings(settings json.RawMessage) (any, error) {
	body := settings
	if len(strings.TrimSpace(string(body))) == 0 {
		body = json.RawMessage(`{}`)
	}
	decoded, err := decode.Decode[a2aSettings](body)
	if err != nil {
		return nil, errdefs.Validationf("a2a: settings: %v", err)
	}
	if strings.TrimSpace(decoded.Peer) == "" {
		return nil, errdefs.Validationf("a2a: settings.peer is required")
	}
	if err := validateBearerAuth(decoded.Auth); err != nil {
		return nil, err
	}
	return decoded, nil
}

// a2aDigest is the identity the peer contributes: where it is and how it is
// reached, without the token.
func a2aDigest(settings json.RawMessage) string {
	var decoded a2aSettings
	_ = json.Unmarshal(settings, &decoded)
	var auth struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(decoded.Auth, &auth)
	return strings.Join([]string{decoded.Peer, auth.Type}, "\x00")
}

func buildA2AEngine(_ context.Context, deps EngineDeps) (agent.RunnerFactory, error) {
	decoded, err := decodeA2ASettings(deps.Spec.Settings)
	if err != nil {
		return nil, err
	}
	settings := decoded.(a2aSettings)
	store, digest := deps.Store, deps.Digest
	return func(ctx context.Context, sessionID, _ string) (agent.Runner, error) {
		return agenta2a.New(ctx, agenta2a.Config{
			Peer:      settings.Peer,
			Auth:      settings.Auth,
			Store:     store,
			Identity:  digest,
			SessionID: sessionID,
		})
	}, nil
}

// validateBearerAuth rejects a credential this build cannot honour. Silently
// dropping it would connect to the peer anonymously and look like an outage.
func validateBearerAuth(auth json.RawMessage) error {
	trimmed := strings.TrimSpace(string(auth))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var header struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(auth, &header); err != nil {
		return errdefs.Validationf("a2a auth: %w", err)
	}
	if header.Type != "" && header.Type != "bearer" {
		return errdefs.Validationf("a2a auth type %q is not supported", header.Type)
	}
	if header.Token == "" {
		return errdefs.Validationf("a2a auth has no token")
	}
	return nil
}
