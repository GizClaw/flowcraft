package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

func openStore(t *testing.T) *SQLite {
	t.Helper()
	store, err := Open(t.TempDir() + "/checkpoints.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func checkpoint(taskID string, round int) agent.Checkpoint {
	return agent.Checkpoint{
		Schema:  agent.CheckpointSchema,
		Runtime: "sha256:x",
		Task:    agent.Task{ID: taskID, ContextID: "conv", State: agent.StateWorking, Round: round},
	}
}

func event(kind, payload string) agent.EventRecord {
	return agent.EventRecord{Kind: kind, Payload: []byte(payload)}
}

// A save advances the projection and appends the round's events together.
func TestSaveAdvancesAndAppends(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	if err := store.Save(ctx, checkpoint("task-1", 0), []agent.EventRecord{event("message", `"a"`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Save(ctx, checkpoint("task-1", 1), []agent.EventRecord{event("message", `"b"`)}); err != nil {
		t.Fatalf("Save 2: %v", err)
	}

	loaded, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Task.Round != 1 {
		t.Errorf("round = %d, want the newest projection", loaded.Task.Round)
	}
	events, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	// The cost of a round is what it added, not the whole transcript: two saves
	// of one event each leave two events, not two copies of everything.
	if events[0].Seq >= events[1].Seq {
		t.Errorf("sequences = %d, %d, want increasing", events[0].Seq, events[1].Seq)
	}
}

func TestLoadMissingTask(t *testing.T) {
	store := openStore(t)
	if _, err := store.Load(context.Background(), "absent"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("err = %v, want ErrNoCheckpoint", err)
	}
}

func TestListAndDelete(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, checkpoint("task-1", 0), nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	summaries, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].ID != "task-1" {
		t.Fatalf("summaries = %+v", summaries)
	}
	if err := store.Delete(ctx, "task-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(ctx, "task-1"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Errorf("task survived deletion: %v", err)
	}
}

// The same bytes must yield the same reference, so repeated tool output costs a
// conflict rather than a write.
func TestPutBlobDeduplicates(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	first, err := store.PutBlob(ctx, []byte("hello"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	second, err := store.PutBlob(ctx, []byte("hello"))
	if err != nil {
		t.Fatalf("PutBlob again: %v", err)
	}
	if first != second {
		t.Errorf("refs differ: %s != %s", first, second)
	}
	body, err := store.GetBlob(ctx, first)
	if err != nil || string(body) != "hello" {
		t.Fatalf("GetBlob = %q, %v", body, err)
	}
	if _, err := store.GetBlob(ctx, "not-a-hash"); err == nil {
		t.Error("malformed reference accepted")
	}
}
