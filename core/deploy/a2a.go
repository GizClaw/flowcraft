package deploy

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
)

// A2AEngine registers the "a2a" engine: a remote agent driven as a local one.
//
// It is a binding, not an implementation: dialing the peer and translating its
// protocol belong to core/agent-a2a, so this package only maps the settings onto
// it, exactly as GraphEngine maps a document onto core/agent-graph.
func A2AEngine() EngineKind {
	return EngineKind{
		Name:        "a2a",
		Decode:      decodeA2ASettings,
		Fingerprint: a2aFingerprint,
		Build:       buildA2AEngine,
	}
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

// a2aFingerprint is the identity the peer contributes: where it is and how it is
// reached, without the token.
func a2aFingerprint(settings json.RawMessage) string {
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
	store, fingerprint := deps.Store, deps.Fingerprint
	return func(ctx context.Context, sessionID, _ string) (agent.Runner, error) {
		return agenta2a.New(ctx, agenta2a.Config{
			Peer:      settings.Peer,
			Auth:      settings.Auth,
			Store:     store,
			Identity:  fingerprint,
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
