package agent_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/goccy/go-json"
)

// The persisted key is part of the storage format: a store decodes the seed
// back into a Checkpoint, so a rename that is not a migration empties every
// fingerprint it holds.
func TestCheckpointFingerprintKey(t *testing.T) {
	body, err := json.Marshal(agent.Checkpoint{Fingerprint: "sha256:x"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(body), `"fingerprint":"sha256:x"`) {
		t.Errorf("encoded checkpoint = %s, want the fingerprint under its own key", body)
	}
	if strings.Contains(string(body), `"runtime"`) {
		t.Errorf("encoded checkpoint = %s, still carries the old key", body)
	}
	var decoded agent.Checkpoint
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Fingerprint != "sha256:x" {
		t.Errorf("round trip = %q, want sha256:x", decoded.Fingerprint)
	}
}
