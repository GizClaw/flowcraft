package agenttest

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/goccy/go-json"
)

// StoreFactory opens a fresh store for each subtest.
type StoreFactory func(t *testing.T) agent.Store

// StoreSuite runs the contract every checkpoint.Store implementation should pass.
//
// It is the store's half of the conformance story: a second backend — a file, Postgres, an
// object store — is verified against this instead of re-deriving what Save and Load mean.
// The assertions are the guarantees a caller relies on, not the fixture's behaviour:
// sequence monotonicity, a projection that carries no transcript, event order, and a
// content-addressed blob reference.
func StoreSuite(t *testing.T, open StoreFactory) {
	t.Helper()
	t.Run("SaveCreatesThenAdvances", func(t *testing.T) { storeAdvances(t, open) })
	t.Run("ProjectionCarriesNoTranscript", func(t *testing.T) { storeProjectionIsSmall(t, open) })
	t.Run("SectionsMergeAcrossSaves", func(t *testing.T) { storeSectionsMerge(t, open) })
	t.Run("EventsKeepTheirOrder", func(t *testing.T) { storeEventOrder(t, open) })
	t.Run("LatestForSessionPicksNewest", func(t *testing.T) { storeLatestForSession(t, open) })
	t.Run("BlobsAreContentAddressed", func(t *testing.T) { storeBlobs(t, open) })
	t.Run("TruncateKeepsTheNewest", func(t *testing.T) { storeTruncate(t, open) })
	t.Run("DeleteRemovesTheTask", func(t *testing.T) { storeDelete(t, open) })
	t.Run("MissingTaskReportsNoCheckpoint", func(t *testing.T) { storeMissing(t, open) })
}

func storeAdvances(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)

	checkpoint := agent.Checkpoint{Fingerprint: "fp-1", Task: agent.Task{ID: "t1", SessionID: "s1", State: agent.StateSubmitted}}
	if _, _, err := store.Save(ctx, checkpoint, nil); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	first, err := store.Load(ctx, "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if first.Version == 0 {
		t.Fatal("the store assigned no version")
	}
	if first.Fingerprint != "fp-1" {
		t.Fatalf("fingerprint = %q, want the one saved", first.Fingerprint)
	}

	checkpoint.Task.State = agent.StateWorking
	if _, _, err := store.Save(ctx, checkpoint, nil); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	second, err := store.Load(ctx, "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if second.Version <= first.Version {
		t.Fatalf("version = %d then %d, want it to advance", first.Version, second.Version)
	}
	if second.Task.State != agent.StateWorking {
		t.Fatalf("state = %q, want the projection to advance", second.Task.State)
	}
}

// storeProjectionSmall is the reason the log exists: a projection that carried history
// would make Load cost the whole transcript, and a thousand-turn task would cost a thousand
// turns to read.
// storeSectionsMerge pins the one write-once rule: a save that omits a section keeps the value the
// previous save wrote. The manifest rides on it — the first save writes it, and no later save repeats
// it — so a store that replaced the map would silently drop the version a trace was produced under.
func storeSectionsMerge(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)

	first := agent.Checkpoint{
		Task:     agent.Task{ID: "t1", SessionID: "s1"},
		Sections: map[string]json.RawMessage{"manifest": json.RawMessage(`{"driver":"graph"}`)},
	}
	if _, _, err := store.Save(ctx, first, nil); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	second := agent.Checkpoint{
		Task:     agent.Task{ID: "t1", SessionID: "s1"},
		Sections: map[string]json.RawMessage{"graph": json.RawMessage(`{"step":1}`)},
	}
	if _, _, err := store.Save(ctx, second, nil); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	stored, err := store.Load(ctx, "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(stored.Sections["manifest"]) != `{"driver":"graph"}` {
		t.Errorf("manifest = %s, want the value the first save wrote", stored.Sections["manifest"])
	}
	if string(stored.Sections["graph"]) != `{"step":1}` {
		t.Errorf("graph = %s, want this save's value", stored.Sections["graph"])
	}
}

func storeProjectionIsSmall(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)

	checkpoint := agent.Checkpoint{Task: agent.Task{ID: "t1", SessionID: "s1"}}
	if _, _, err := store.Save(ctx, checkpoint, []agent.EventRecord{{Kind: "message", Payload: []byte(`"hi"`)}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(ctx, "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The projection carries where the run stands, and the facts beside it are the log: a
	// transcript in the projection would be rewritten on every save.
	stored, err := store.Events(ctx, "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(stored) != 1 || stored[0].Kind != "message" {
		t.Fatalf("events = %+v, want the one fact that was saved", stored)
	}
	if loaded.Task.ID != "t1" || loaded.Task.SessionID != "s1" {
		t.Fatalf("projection = %+v, want the task's identity", loaded.Task)
	}
}

func storeEventOrder(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)

	checkpoint := agent.Checkpoint{Task: agent.Task{ID: "t1", SessionID: "s1"}}
	first, last, err := store.Save(ctx, checkpoint, []agent.EventRecord{{Kind: "a"}, {Kind: "b"}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if first == 0 || last != first+1 {
		t.Fatalf("sequence range = %d..%d, want two consecutive numbers", first, last)
	}
	events, err := store.Events(ctx, "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 || events[0].Kind != "a" || events[1].Kind != "b" {
		t.Fatalf("events = %+v, want [a b] in order", events)
	}
	if events[0].Seq >= events[1].Seq {
		t.Fatalf("sequence numbers are not increasing: %d then %d", events[0].Seq, events[1].Seq)
	}

	after, err := store.EventsAfter(ctx, "t1", first)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(after) != 1 || after[0].Kind != "b" {
		t.Fatalf("EventsAfter = %+v, want only the later event", after)
	}
}

func storeLatestForSession(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)
	for _, id := range []string{"t1", "t2"} {
		if _, _, err := store.Save(ctx, agent.Checkpoint{Task: agent.Task{ID: id, SessionID: "s1"}},
			[]agent.EventRecord{{Kind: "message"}}); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}
	latest, err := store.LatestForSession(ctx, "s1")
	if err != nil {
		t.Fatalf("LatestForSession: %v", err)
	}
	if latest.Task.ID != "t2" {
		t.Fatalf("latest = %q, want the newest task of the session", latest.Task.ID)
	}
	if _, err := store.LatestForSession(ctx, "nobody"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("LatestForSession(unknown) = %v, want ErrNoCheckpoint", err)
	}
}

func storeBlobs(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)

	first, err := store.PutBlob(ctx, []byte("payload"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	again, err := store.PutBlob(ctx, []byte("payload"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if first != again {
		t.Fatalf("the same bytes gave %q then %q; a reference must be content-addressed", first, again)
	}
	got, err := store.GetBlob(ctx, first)
	if err != nil || string(got) != "payload" {
		t.Fatalf("GetBlob = %q, %v", got, err)
	}
	if _, err := store.GetBlob(ctx, "sha256:nothing"); err == nil {
		t.Fatal("GetBlob returned a nil error for an unknown reference")
	}
}

// storeTruncate pins the one invariant a compaction cannot break: the newest event stays,
// so the next save continues after the tail instead of reusing a number.
func storeTruncate(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)

	if _, _, err := store.Save(ctx, agent.Checkpoint{Task: agent.Task{ID: "t1", SessionID: "s1"}},
		[]agent.EventRecord{{Kind: "state"}, {Kind: "trace"}, {Kind: "trace"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.TruncateEvents(ctx, "t1", 1_000); err != nil {
		t.Fatalf("TruncateEvents: %v", err)
	}
	events, err := store.Events(ctx, "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events after truncation = %d, want only the newest", len(events))
	}

	// A second save must not reuse the dropped numbers.
	first, _, err := store.Save(ctx, agent.Checkpoint{Task: agent.Task{ID: "t1", SessionID: "s1"}},
		[]agent.EventRecord{{Kind: "state"}})
	if err != nil {
		t.Fatalf("Save after truncation: %v", err)
	}
	if first <= events[0].Seq {
		t.Fatalf("new event seq = %d, want it after %d", first, events[0].Seq)
	}
}

func storeDelete(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)
	if _, _, err := store.Save(ctx, agent.Checkpoint{Task: agent.Task{ID: "t1", SessionID: "s1"}}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(ctx, "t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(ctx, "t1"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("Load after Delete = %v, want ErrNoCheckpoint", err)
	}
}

func storeMissing(t *testing.T, open StoreFactory) {
	t.Helper()
	ctx := context.Background()
	store := open(t)
	if _, err := store.Load(ctx, "nothing"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("Load(missing) = %v, want ErrNoCheckpoint", err)
	}
}
