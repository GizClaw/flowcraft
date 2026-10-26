package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/backends/checkpoint/sqlite"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// newStore returns a fresh in-memory store. A store opened with [sqlite.Open]
// serialises through a single connection, which is what makes ":memory:" behave
// like a file.
func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newStoreOnDB returns a store over a handle the test also keeps, so a test can
// corrupt the chain the way a partial write or a manual cleanup would.
func newStoreOnDB(t *testing.T) (*sqlite.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := sqlite.New(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store, db
}

func TestStoreContract(t *testing.T) {
	agenttest.CheckpointStoreSuite(t, func() agent.CheckpointStore { return newStore(t) })
}

func TestJournalContract(t *testing.T) {
	agenttest.CheckpointJournalSuite(t, func() agent.CheckpointStore { return newStore(t) })
}

// TestLoadRejectsBrokenChain proves a gap is reported as an unavailable-class
// error rather than silently resumed from partial state: the host's cue is to
// start a fresh run.
func TestLoadRejectsBrokenChain(t *testing.T) {
	store, db := newStoreOnDB(t)
	ctx := context.Background()
	journal := journalFor(t, store)

	if err := journal.WriteBase(ctx, agenttest.JournalBase("run-1")); err != nil {
		t.Fatalf("WriteBase: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if err := journal.Append(ctx, agenttest.JournalDelta("run-1", i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	// Drop the first delta behind the store's back, leaving seq 2 and 3
	// without their predecessor.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM agent_checkpoint_deltas WHERE exec_id = ? AND seq = 1`, "run-1"); err != nil {
		t.Fatalf("corrupt chain: %v", err)
	}

	if _, err := store.Load(ctx, "run-1"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Load over a broken chain = %v, want not-available", err)
	}
}

// TestChainSurvivesReopen proves the deltas are durable, not an in-memory
// optimisation: a new store over the same file replays them.
func TestChainSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	journal := journalFor(t, store)
	if err := journal.WriteBase(ctx, agenttest.JournalBase("run-1")); err != nil {
		t.Fatalf("WriteBase: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if err := journal.Append(ctx, agenttest.JournalDelta("run-1", i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.Load(ctx, "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load after reopen returned nil")
	}
	if got.Iteration != 3 {
		t.Fatalf("Iteration = %d, want the last appended boundary", got.Iteration)
	}
	msgs := got.Board.Channels[agent.MainChannel]
	if len(msgs) != 1+3 {
		t.Fatalf("main channel = %d messages, want base + 3 appends", len(msgs))
	}
	if last := msgs[len(msgs)-1].Content.Text(); last != "delta-3" {
		t.Fatalf("last message = %q, want delta-3", last)
	}
	if step, ok := got.Board.Vars["step"]; !ok || step != float64(3) {
		t.Fatalf("var step = %v, want 3 from the last delta", step)
	}
}

// TestMaxDeltasAsksForBase proves a store-side cap makes the writer persist a
// self-contained record instead of failing.
func TestMaxDeltasAsksForBase(t *testing.T) {
	store, err := sqlite.Open(":memory:", sqlite.WithMaxDeltas(2))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	journal := journalFor(t, store)
	if err := journal.WriteBase(ctx, agenttest.JournalBase("run-1")); err != nil {
		t.Fatalf("WriteBase: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := journal.Append(ctx, agenttest.JournalDelta("run-1", i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := journal.Append(ctx, agenttest.JournalDelta("run-1", 3)); !errors.Is(err, agent.ErrNeedsBase) {
		t.Fatalf("Append past the cap = %v, want ErrNeedsBase", err)
	}
}

// TestDeleteClearsChain proves deleting a record takes its chain with it, so a
// later record reusing the exec id cannot resume against an orphaned delta.
func TestDeleteClearsChain(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	journal := journalFor(t, store)
	if err := journal.WriteBase(ctx, agenttest.JournalBase("run-1")); err != nil {
		t.Fatalf("WriteBase: %v", err)
	}
	if err := journal.Append(ctx, agenttest.JournalDelta("run-1", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Delete(ctx, "run-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := store.Load(ctx, "run-1")
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("Load after delete = %+v, want nil", got)
	}
	if err := journal.WriteBase(ctx, agenttest.JournalBase("run-1")); err != nil {
		t.Fatalf("WriteBase after delete: %v", err)
	}
	got, err = store.Load(ctx, "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if msgs := got.Board.Channels[agent.MainChannel]; len(msgs) != 1 {
		t.Fatalf("main channel = %d messages, want only the new base", len(msgs))
	}
}

func journalFor(t *testing.T, store agent.CheckpointStore) agent.CheckpointJournal {
	t.Helper()
	journal, ok := store.(agent.CheckpointJournal)
	if !ok {
		t.Fatalf("store %T does not implement agent.CheckpointJournal", store)
	}
	return journal
}
