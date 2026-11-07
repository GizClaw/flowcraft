package deploy

import (
	"context"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/runtime"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// engine is what an agent runs on. It opens one conversation at a time; the
// manager owns lifetime and concurrency, the engine owns construction.
type engine interface {
	Open(ctx context.Context, conversationID string) (session, error)
	Close() error
}

// Engine registers one engine kind.
//
// The build function receives the agent under construction because an engine may
// need the agent's runtime, options or store; nothing is half-built that the
// manager cannot see.
type Engine struct {
	Name  string
	Build func(ctx context.Context, spec EngineSpec, agent *Agent) (engine, error)
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
	Decode func(settings map[string]any) (any, error)
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

// reactiveEngine opens the local loop over the agent's runtime.
type reactiveEngine struct{ agent *Agent }

func (e reactiveEngine) Open(ctx context.Context, conversationID string) (session, error) {
	return e.agent.openLocalSession(ctx, conversationID)
}

func (reactiveEngine) Close() error { return nil }

// proxyEngine opens conversations that forward to a remote A2A agent.
type proxyEngine struct {
	spec   EngineSpec
	store  agent.Store
	digest runtime.Digest
}

func (e proxyEngine) Open(ctx context.Context, conversationID string) (session, error) {
	return newProxyConversation(ctx, e.spec, e.store, e.digest, conversationID)
}

func (proxyEngine) Close() error { return nil }

// builtinEngines are registered on every manager: a host that wants fewer
// registers its own after New, and New's defaults stay out of the way.
func builtinEngines() map[string]Engine {
	return map[string]Engine{
		"react": {
			Name: "react",
			Build: func(_ context.Context, _ EngineSpec, agent *Agent) (engine, error) {
				return reactiveEngine{agent: agent}, nil
			},
		},
		"a2a": {
			Name: "a2a",
			Build: func(_ context.Context, spec EngineSpec, deployed *Agent) (engine, error) {
				if strings.TrimSpace(spec.Peer) == "" {
					return nil, errdefs.Validationf("manage: the a2a engine needs a peer")
				}
				return proxyEngine{spec: spec, store: deployed.spec.Store, digest: deployed.digest}, nil
			},
		},
	}
}

// EngineNames lists the registered engine kinds, sorted.
func (m *Manager) EngineNames() []string { return sortedKeys(m.engines) }

// ProtocolNames lists the registered protocol kinds, sorted.
func (m *Manager) ProtocolNames() []string { return sortedKeys(m.protocols) }
