package checkpoint

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
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

// The durability mode is a store decision; the option must actually reach the
// connection, not just the struct.
func TestSynchronousModeIsApplied(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
		want int
	}{
		{"default", nil, 2}, // FULL
		{"normal", []Option{WithSynchronous(SynchronousNormal)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir()+"/cp.db", tc.opts...)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			var mode int
			if err := store.db.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil {
				t.Fatalf("read synchronous: %v", err)
			}
			if mode != tc.want {
				t.Errorf("synchronous = %d, want %d", mode, tc.want)
			}
		})
	}
	if _, err := Open(t.TempDir()+"/cp.db", WithSynchronous(9)); err == nil {
		t.Fatal("an unknown synchronous mode was accepted")
	}
}

func checkpoint(taskID string, round int) agent.Checkpoint {
	return agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:x",
		Task:        agent.Task{ID: taskID, SessionID: "conv", State: agent.StateWorking, Round: round},
	}
}

func event(kind, payload string) agent.EventRecord {
	return agent.EventRecord{Kind: kind, Payload: []byte(payload)}
}

// A save advances the projection and appends the round's events together.
func TestSaveAdvancesAndAppends(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	if _, _, err := store.Save(ctx, checkpoint("task-1", 0), []agent.EventRecord{event("message", `"a"`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := store.Save(ctx, checkpoint("task-1", 1), []agent.EventRecord{event("message", `"b"`)}); err != nil {
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
	if _, _, err := store.Save(ctx, checkpoint("task-1", 0), nil); err != nil {
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

// History after the seed comes from the event log, so the projection row does
// not grow with it: n rounds cost O(n), not O(n^2).
func TestProjectionDoesNotCarryHistory(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	seed := checkpoint("task-1", 0)
	seed.Task.History = []message.Message{
		message.NewTextMessage(message.RoleUser, "write the report"),
	}
	if _, _, err := store.Save(ctx, seed, nil); err != nil {
		t.Fatalf("Save seed: %v", err)
	}
	bodySize := func() int {
		var n int
		if err := store.db.QueryRowContext(ctx,
			`SELECT length(body) FROM tasks WHERE task_id = ?`, "task-1").Scan(&n); err != nil {
			t.Fatalf("body size: %v", err)
		}
		return n
	}
	seeded := bodySize()

	for round := 1; round <= 50; round++ {
		round1 := message.NewTextMessage(message.RoleAssistant, strings.Repeat("x", 200))
		payload, err := json.Marshal(round1)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		cp := checkpoint("task-1", round)
		cp.Task.History = append(seed.Task.History, round1)
		if _, _, err := store.Save(ctx, cp, []agent.EventRecord{{Kind: "message", Payload: payload}}); err != nil {
			t.Fatalf("Save round %d: %v", round, err)
		}
	}
	if grown := bodySize(); grown > seeded+16 {
		t.Errorf("projection body grew from %d to %d bytes; history leaked into it", seeded, grown)
	}

	loaded, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Load is the projection only: the 50 messages are in the event log.
	if len(loaded.Task.History) != 0 {
		t.Fatalf("history = %d messages, want the projection to carry none",
			len(loaded.Task.History))
	}
	events, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 50 {
		t.Fatalf("events = %d, want the 50 messages", len(events))
	}
}

// The SQLite backend must satisfy the same contract every other backend does;
// these are the promises a resume relies on, not SQLite details.
func TestSQLiteConformance(t *testing.T) {
	agenttest.Store(t, func(t *testing.T) agent.Store {
		return openStore(t)
	})
}

// A database written before events carried a time must still open, and its rows
// must read back undated rather than dated at the epoch. The alternative — a
// schema check that refuses — would turn an upgrade into a data migration, which
// is exactly what this store's swallow-the-error style avoids.
func TestOpenMigratesAnEventsTableWithoutCreatedAt(t *testing.T) {
	path := t.TempDir() + "/old.db"
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// The shape this store wrote before the column existed.
	if _, err := old.Exec(`
CREATE TABLE tasks (
	task_id TEXT PRIMARY KEY, context_id TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL, round INTEGER NOT NULL DEFAULT 0,
	schema INTEGER NOT NULL, fingerprint TEXT NOT NULL,
	body BLOB NOT NULL, version INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL
);
CREATE TABLE events (
	task_id TEXT NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL,
	payload BLOB NOT NULL, PRIMARY KEY (task_id, seq)
);
INSERT INTO events (task_id, seq, kind, payload) VALUES ('task-1', 1, 'message', '{"old":true}');
`); err != nil {
		_ = old.Close()
		t.Fatalf("seed the old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open the old database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	events, err := store.Events(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want the row written before the migration", len(events))
	}
	if !events[0].At.IsZero() {
		t.Errorf("an old row came back dated %v, want unknown", events[0].At)
	}
	// And the column is usable afterwards: a new save carries its own time.
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	if _, _, err := store.Save(context.Background(),
		agent.Checkpoint{Schema: agent.CheckpointSchema, Task: agent.Task{ID: "task-1"}},
		[]agent.EventRecord{{Kind: "message", Payload: json.RawMessage(`{}`), At: at}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	appended, err := store.EventsAfter(context.Background(), "task-1", events[0].Seq)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(appended) != 1 || !appended[0].At.Equal(at) {
		t.Fatalf("appended = %+v, want the named time", appended)
	}
}
