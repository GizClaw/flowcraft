package agent_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

// TestTaskStateVocabulary pins the two predicates a caller branches on: which states end
// a task, and which are legal at all.
func TestTaskStateVocabulary(t *testing.T) {
	terminal := map[agent.TaskState]bool{
		agent.StateSubmitted:     false,
		agent.StateWorking:       false,
		agent.StateInputRequired: false,
		agent.StateAuthRequired:  false,
		agent.StateCompleted:     true,
		agent.StateFailed:        true,
		agent.StateCanceled:      true,
		agent.StateRejected:      true,
	}
	for state, want := range terminal {
		if got := state.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", state, got, want)
		}
		if !state.Valid() {
			t.Errorf("%s.Valid() = false", state)
		}
	}
	if agent.TaskState("paused").Valid() {
		t.Error("an unknown state reported itself valid")
	}
}

// TestManifestIDIsStable keeps two runs comparable: the same version combination must
// hash the same, and a zero manifest must have no identity rather than a hash of nothing.
func TestManifestIDIsStable(t *testing.T) {
	first := agent.Manifest{Deployment: "fp-1", Driver: "graph", Models: "m1"}
	second := agent.Manifest{Deployment: "fp-1", Driver: "graph", Models: "m1"}
	if first.ID() == "" || first.ID() != second.ID() {
		t.Fatalf("IDs differ: %q vs %q", first.ID(), second.ID())
	}
	third := agent.Manifest{Deployment: "fp-2", Driver: "graph", Models: "m1"}
	if third.ID() == first.ID() {
		t.Fatal("a different deployment hashed the same")
	}
	if got := (agent.Manifest{}).ID(); got != "" {
		t.Fatalf("the zero manifest has identity %q", got)
	}
}

// TestManifestSchemaChangesTheIdentity is why the schema field exists: an id is written into
// checkpoints and run records, so changing how one is computed must be a visible new version
// rather than the same name quietly meaning something else.
func TestManifestSchemaChangesTheIdentity(t *testing.T) {
	current := agent.Manifest{Schema: agent.ManifestSchema, Deployment: "fp-1", Driver: "graph"}
	next := current
	next.Schema = "manifest/2"
	if current.ID() == next.ID() {
		t.Fatal("a new digest schema produced the same identity")
	}
	if agent.ManifestSchema == "" {
		t.Fatal("the manifest has no schema version")
	}
}
