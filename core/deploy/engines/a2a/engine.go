// Package a2a registers the "a2a" engine with a deployment: a remote agent
// driven as a local one.
//
// It is the opt-in half of core/deploy's kind table, split out for the reason
// core/runtime/backends is: a host that never drives a peer should not link the
// A2A client, and core/deploy itself imports no engine at all.
//
// The registration is a binding, not an implementation: dialing the peer and
// translating its protocol belong to core/agent-a2a, so this package only maps
// the settings onto it, exactly as the graph engine maps a document onto
// core/agent-graph.
package a2a

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/goccy/go-json"
)

// Engine returns the "a2a" engine kind.
func Engine() deploy.EngineKind {
	return deploy.EngineKind{
		Name: "a2a",
		// A peer is described entirely by its settings: there is no document to
		// read, so the source table goes unused.
		Decode: func(_ context.Context, settings json.RawMessage, _ deploy.SourceOpener) (any, error) {
			return decodeSettings(settings)
		},
		Fingerprint: func(_ context.Context, settings json.RawMessage, _ deploy.SourceOpener) string {
			return fingerprint(settings)
		},
		Build: build,
	}
}

// a2aSettings is the engine's written configuration.
type a2aSettings struct {
	Peer string          `json:"peer"`
	Auth json.RawMessage `json:"auth"`
}

func decodeSettings(settings json.RawMessage) (any, error) {
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

// fingerprint is the identity the peer contributes: where it is and how it is
// reached, without the token.
func fingerprint(settings json.RawMessage) string {
	var decoded a2aSettings
	if err := json.Unmarshal(settings, &decoded); err != nil {
		// Decode refuses a settings blob that does not parse, so this is
		// unreachable through the loader. If it is reached anyway, the identity
		// must differ from every real peer's rather than look like an empty one.
		return "invalid"
	}
	var auth struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(decoded.Auth, &auth); err != nil {
		// An auth block that does not parse was already refused by Decode; the
		// peer's location alone is still its identity.
		auth.Type = ""
	}
	return strings.Join([]string{decoded.Peer, auth.Type}, "\x00")
}

func build(_ context.Context, deps deploy.EngineDeps) (agent.RunnerFactory, error) {
	decoded, err := decodeSettings(deps.Spec.Settings)
	if err != nil {
		return nil, err
	}
	settings := decoded.(a2aSettings)
	store, identity := deps.Store, deps.Fingerprint
	return func(ctx context.Context, sessionID, _ string) (agent.Runner, error) {
		return agenta2a.New(ctx, agenta2a.Config{
			Peer:      settings.Peer,
			Auth:      settings.Auth,
			Store:     store,
			Identity:  identity,
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
