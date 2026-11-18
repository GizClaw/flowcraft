package a2a_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	enginesa2a "github.com/GizClaw/flowcraft/core/deploy/engines/a2a"
	"github.com/goccy/go-json"
)

func rawSettings(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

// An auth block this build cannot honour is refused instead of being dropped:
// connecting anonymously would look like a peer outage.
func TestDecodeRejectsUnsupportedAuth(t *testing.T) {
	decode := enginesa2a.Engine().Decode
	for name, auth := range map[string]json.RawMessage{
		"type":  json.RawMessage(`{"type":"basic","token":"secret"}`),
		"empty": json.RawMessage(`{"type":"bearer"}`),
		"junk":  json.RawMessage(`"not an object"`),
	} {
		t.Run(name, func(t *testing.T) {
			settings := rawSettings(map[string]any{"peer": "https://peer.test", "auth": auth})
			if _, err := decode(settings); err == nil || !strings.Contains(err.Error(), "a2a auth") {
				t.Fatalf("Decode = %v, want an auth complaint", err)
			}
		})
	}
}

// A bearer credential is the shape this build honours, so it must be accepted.
func TestDecodeAcceptsBearerAuth(t *testing.T) {
	settings := rawSettings(map[string]any{
		"peer": "https://peer.test",
		"auth": map[string]string{"type": "bearer", "token": "secret"},
	})
	if _, err := enginesa2a.Engine().Decode(settings); err != nil {
		t.Fatalf("Decode = %v, want the bearer credential accepted", err)
	}
}

// A missing peer is refused at load time, not at the first message.
func TestDecodeRequiresAPeer(t *testing.T) {
	if _, err := enginesa2a.Engine().Decode(json.RawMessage(`{}`)); err == nil {
		t.Fatal("an a2a engine without a peer was accepted")
	}
}

// Settings are decoded strictly: a typo must fail the load, not be silently
// dropped on the way to the peer.
func TestDecodeRejectsUnknownSettings(t *testing.T) {
	settings := rawSettings(map[string]any{"peer": "https://peer.test", "bogus": 1})
	_, err := enginesa2a.Engine().Decode(settings)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("Decode = %v, want a complaint about the unknown setting", err)
	}
}

// The token is not part of the deployment's identity: rotating a credential must
// not invalidate a session, changing the peer must.
func TestFingerprintIgnoresTheToken(t *testing.T) {
	fingerprint := enginesa2a.Engine().Fingerprint
	one := fingerprint(rawSettings(map[string]any{"peer": "https://a", "auth": map[string]string{"type": "bearer", "token": "one"}}))
	two := fingerprint(rawSettings(map[string]any{"peer": "https://a", "auth": map[string]string{"type": "bearer", "token": "two"}}))
	if one != two {
		t.Error("rotating the token changed the fingerprint")
	}
	other := fingerprint(rawSettings(map[string]any{"peer": "https://b"}))
	if one == other {
		t.Error("changing the peer did not change the fingerprint")
	}
}

// The registration is what a document names, so it has to reach through the
// loader: a kind that loads and a kind that deploys are the same value.
func TestRegistrationLoadsADocument(t *testing.T) {
	loader := deploy.Loader{Kinds: deploy.Kinds{}.WithEngines(enginesa2a.Engine())}
	specs, err := loader.Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine:
      kind: a2a
      settings: {peer: https://peer.example}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if specs[0].Engine.Kind != "a2a" {
		t.Errorf("engine = %+v, want the a2a kind", specs[0].Engine)
	}
}

// A document naming the engine still fails when the host registered nothing: the
// error names what is offered, which is the whole point of not shipping a
// hardcoded table.
func TestUnregisteredEngineIsRefused(t *testing.T) {
	loader := deploy.Loader{Kinds: deploy.Kinds{}}
	_, err := loader.Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine: {kind: a2a, settings: {peer: https://peer.example}}
`))
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("Load = %v, want an unregistered-kind complaint", err)
	}
}
