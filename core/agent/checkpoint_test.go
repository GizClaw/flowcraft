package agent_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
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

// TestDriverStoreOfIsTheDriversHalf pins the narrowing: what a driver is handed delegates the
// operations a run performs, including the compaction option that belongs to the save making a
// base durable, and stops there.
func TestDriverStoreOfIsTheDriversHalf(t *testing.T) {
	full := agenttest.NewMemStore()
	store := agent.DriverStoreOf(full)
	ctx := context.Background()

	first, last, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.Schema,
		Task:   agent.Task{ID: "t1", SessionID: "s1", State: agent.StateWorking},
	}, []agent.EventRecord{{Kind: "message", Payload: []byte(`"hi"`)}})
	if err != nil || first != 1 || last != 1 {
		t.Fatalf("Save = %d..%d, %v", first, last, err)
	}

	events, err := store.EventsAfter(ctx, "t1", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("EventsAfter = %+v, %v", events, err)
	}

	latest, err := store.LatestForSession(ctx, "s1")
	if err != nil || latest.Task.ID != "t1" {
		t.Fatalf("LatestForSession = %+v, %v", latest, err)
	}

	// The compaction option has to survive the narrowing: it is the reason Save takes options
	// at all, and a driver that lost it would pay two commits for one decision.
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.Schema,
		Task:   agent.Task{ID: "t1", SessionID: "s1", State: agent.StateWorking, Round: 1},
	}, []agent.EventRecord{{Kind: "graph.state", Payload: []byte(`{}`)}},
		agent.TruncateThrough(1, "message")); err != nil {
		t.Fatalf("Save with truncation: %v", err)
	}
	events, err = store.EventsAfter(ctx, "t1", 0)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "graph.state" {
		t.Fatalf("events = %+v, want the truncated log to keep only the newest kind", events)
	}

	ref, err := store.PutBlob(ctx, []byte("bytes"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	body, err := store.GetBlob(ctx, ref)
	if err != nil || string(body) != "bytes" {
		t.Fatalf("GetBlob = %q, %v", body, err)
	}

	if agent.DriverStoreOf(nil) != nil {
		t.Fatal("a nil store narrowed to a usable one")
	}
}
