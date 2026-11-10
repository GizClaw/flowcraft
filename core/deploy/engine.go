package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// RunnerFactory builds one conversation's engine.
//
// It is exactly agent.AgentOptions.NewRunner: an engine is a runner factory, not
// a long-lived object the manager drives.
type RunnerFactory func(ctx context.Context, conversationID, taskID string) (agent.Runner, error)

// EngineDeps is what an engine kind is built from: the deployment's engine block
// and the substrate the rest of the deployment already assembled.
type EngineDeps struct {
	Spec    EngineSpec
	Runtime *runtime.Runtime
	Store   agent.Store
	Digest  runtime.Digest
}

// Engine registers one engine kind.
type Engine struct {
	Name string
	// Local marks an engine that runs in this process: it needs a runtime and
	// takes no peer. A remote engine is the opposite, and the loader validates
	// each kind by what it says here instead of by its name.
	Local bool
	// Validate applies the kind's own field rules at load time. It runs after
	// the shared Local/peer/runtime checks, so a kind only speaks for the fields
	// it owns.
	Validate func(EngineSpec) error
	Build    func(ctx context.Context, deps EngineDeps) (RunnerFactory, error)
}

// validateEngineSpec applies the rules every kind shares and then the kind's
// own: a local engine has a runtime and no peer, a remote one the reverse.
func validateEngineSpec(registration Engine, spec EngineSpec, hasRuntime bool) error {
	if registration.Local {
		if !hasRuntime {
			return errdefs.Validationf("kind %q needs a runtime", spec.Kind)
		}
		if strings.TrimSpace(spec.Peer) != "" {
			return errdefs.Validationf("kind %q has no peer", spec.Kind)
		}
	} else {
		if hasRuntime {
			return errdefs.Validationf("kind %q has no local runtime to configure", spec.Kind)
		}
		if strings.TrimSpace(spec.Peer) == "" {
			return errdefs.Validationf("kind %q needs a peer", spec.Kind)
		}
	}
	if registration.Validate != nil {
		return registration.Validate(spec)
	}
	return nil
}

// protocol is an agent's external surface.
type protocol interface {
	Handler() http.Handler
	Close() error
}

// Protocol registers one protocol kind.
type Protocol struct {
	Name string
	// Decode validates the written settings and returns the typed value Build
	// consumes. It runs in the loader, so a deployment that forgets its card
	// fails before any resource is built.
	Decode func(settings json.RawMessage) (any, error)
	Build  func(cfg ProtocolSpec, agent *Agent) (protocol, error)
}

// WithEngines registers engine kinds.
func WithEngines(engines ...Engine) ManagerOption {
	return func(m *Manager) {
		for _, item := range engines {
			m.engines[item.Name] = item
		}
	}
}

// WithProtocols registers protocol kinds.
func WithProtocols(protocols ...Protocol) ManagerOption {
	return func(m *Manager) {
		for _, item := range protocols {
			m.protocols[item.Name] = item
		}
	}
}

// builtinEngines are registered on every manager: a host that wants fewer
// registers its own after New, and New's defaults stay out of the way.
//
// The a2a engine is the client half of the protocol: a remote agent is driven
// through the same contract a local engine implements. The graph engine compiles
// one document and serves every conversation from it.
func builtinEngines() map[string]Engine {
	return map[string]Engine{
		"a2a": {
			Name: "a2a",
			Validate: func(spec EngineSpec) error {
				if strings.TrimSpace(spec.Peer) == "" {
					return errdefs.Validationf("the a2a engine needs a peer")
				}
				return validateBearerAuth(spec.Auth)
			},
			Build: func(_ context.Context, deps EngineDeps) (RunnerFactory, error) {
				if strings.TrimSpace(deps.Spec.Peer) == "" {
					return nil, errdefs.Validationf("manage: the a2a engine needs a peer")
				}
				peer, auth := deps.Spec.Peer, deps.Spec.Auth
				store, digest := deps.Store, deps.Digest
				return func(ctx context.Context, conversationID, _ string) (agent.Runner, error) {
					return agenta2a.New(ctx, agenta2a.Config{
						Peer:           peer,
						Auth:           auth,
						Store:          store,
						Identity:       digest,
						ConversationID: conversationID,
					})
				}, nil
			},
		},
	}
}

// EngineNames lists the registered engine kinds, sorted.
func (m *Manager) EngineNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedKeys(m.engines)
}

// ProtocolNames lists the registered protocol kinds, sorted.
func (m *Manager) ProtocolNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedKeys(m.protocols)
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
