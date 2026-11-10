package deploy

import (
	"encoding/json"
	"strings"
	"testing"
)

// An auth block this build cannot honour is refused instead of being dropped:
// connecting anonymously would look like a peer outage.
func TestA2AEngineDecodeRejectsUnsupportedAuth(t *testing.T) {
	decode := BuiltinKinds().Engines["a2a"].Decode
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

// A missing peer is refused at load time, not at the first message.
func TestA2AEngineDecodeRequiresAPeer(t *testing.T) {
	if _, err := BuiltinKinds().Engines["a2a"].Decode(json.RawMessage(`{}`)); err == nil {
		t.Fatal("an a2a engine without a peer was accepted")
	}
}

// The token is not part of the deployment's identity: rotating a credential must
// not invalidate a session, changing the peer must.
func TestA2ADigestIgnoresTheToken(t *testing.T) {
	digest := BuiltinKinds().Engines["a2a"].Digest
	one := digest(rawSettings(map[string]any{"peer": "https://a", "auth": map[string]string{"type": "bearer", "token": "one"}}))
	two := digest(rawSettings(map[string]any{"peer": "https://a", "auth": map[string]string{"type": "bearer", "token": "two"}}))
	if one != two {
		t.Error("rotating the token changed the digest")
	}
	other := digest(rawSettings(map[string]any{"peer": "https://b"}))
	if one == other {
		t.Error("changing the peer did not change the digest")
	}
}

// Kinds are a value: extending one must not mutate what BuiltinKinds handed out.
func TestKindsWithEnginesDoesNotMutate(t *testing.T) {
	base := BuiltinKinds()
	extended := base.WithEngines(engineFor("extra", nil))
	if _, ok := base.Engines["extra"]; ok {
		t.Fatal("WithEngines mutated the receiver")
	}
	if _, ok := extended.Engines["extra"]; !ok {
		t.Fatal("WithEngines did not add the kind")
	}
}
