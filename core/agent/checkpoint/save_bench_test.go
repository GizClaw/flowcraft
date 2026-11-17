package checkpoint

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

func benchStore(b *testing.B, config ...Option) *SQLite {
	b.Helper()
	store, err := Open(b.TempDir()+"/checkpoints.db", config...)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

func longHistory(n int) []message.Message {
	messages := make([]message.Message, n)
	for i := range messages {
		messages[i] = message.NewTextMessage(message.RoleUser, "a message in a long session")
	}
	return messages
}

// BenchmarkSaveWithHistory is what an adapter pays when it hands the projection
// back with the history it reconstructed: the seed is written only once, but the
// checkpoint is marshalled on every save.
func BenchmarkSaveWithHistory(b *testing.B) {
	for _, size := range []int{0, 1000, 10000} {
		b.Run(fmt.Sprintf("messages=%d", size), func(b *testing.B) {
			store := benchStore(b)
			ctx := context.Background()
			checkpoint := agent.Checkpoint{
				Schema: agent.CheckpointSchema,
				Task: agent.Task{
					ID: "t1", SessionID: "c1",
					State:   agent.StateWorking,
					History: longHistory(size),
				},
			}
			if _, _, err := store.Save(ctx, checkpoint, nil); err != nil {
				b.Fatalf("seed save: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, _, err := store.Save(ctx, checkpoint, nil); err != nil {
					b.Fatalf("save: %v", err)
				}
			}
		})
	}
}

// BenchmarkSaveDelta is the shape an engine should use: the projection carries no
// history, the step's writes are events, and the cost does not grow with the
// session. The sub-benchmarks are the store's durability choice.
func BenchmarkSaveDelta(b *testing.B) {
	for _, mode := range syncModes {
		b.Run(mode.name, func(b *testing.B) {
			store := benchStore(b, mode.opts...)
			ctx := context.Background()
			checkpoint := agent.Checkpoint{
				Schema: agent.CheckpointSchema,
				Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateWorking},
			}
			body, err := json.Marshal(message.NewTextMessage(message.RoleUser, "one turn"))
			if err != nil {
				b.Fatalf("encode: %v", err)
			}
			events := []agent.EventRecord{{Kind: "message", Payload: body}}
			if _, _, err := store.Save(ctx, checkpoint, events[:0]); err != nil {
				b.Fatalf("seed save: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, _, err := store.Save(ctx, checkpoint, events); err != nil {
					b.Fatalf("save: %v", err)
				}
			}
		})
	}
}

// A save writes its events as chunked multi-row INSERTs; the rows must be what
// one INSERT per event produced, including the sequence range and created_at.
// The sizes straddle eventInsertRows on both sides.
func TestSaveAppendsEventsInChunks(t *testing.T) {
	for _, size := range []int{80, 81, 160, 161, 200} {
		t.Run(fmt.Sprintf("events=%d", size), func(t *testing.T) {
			store := openStore(t)
			ctx := context.Background()
			events := make([]agent.EventRecord, size)
			for i := range events {
				events[i] = agent.EventRecord{
					Kind:    fmt.Sprintf("kind-%d", i%3),
					Payload: []byte(fmt.Sprintf(`{"i":%d}`, i)),
					At:      time.Unix(1_700_000_000, int64(i)).UTC(),
				}
			}

			first, last, err := store.Save(ctx, checkpoint("task-1", 1), events)
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if first != 1 || last != int64(size) {
				t.Fatalf("range = %d..%d, want 1..%d", first, last, size)
			}

			loaded, err := store.Load(ctx, "task-1")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if loaded.Task.ID != "task-1" || loaded.Task.Round != 1 || loaded.Task.State != agent.StateWorking {
				t.Errorf("projection = %+v, want the saved one", loaded.Task)
			}

			stored, err := store.Events(ctx, "task-1")
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			if len(stored) != size {
				t.Fatalf("events = %d, want %d", len(stored), size)
			}
			for i, record := range stored {
				want := events[i]
				if record.Seq != int64(i+1) {
					t.Fatalf("event %d seq = %d, want %d", i, record.Seq, i+1)
				}
				if record.Kind != want.Kind {
					t.Errorf("event %d kind = %q, want %q", i, record.Kind, want.Kind)
				}
				if string(record.Payload) != string(want.Payload) {
					t.Errorf("event %d payload = %s, want %s", i, record.Payload, want.Payload)
				}
				if !record.At.Equal(want.At) {
					t.Errorf("event %d at = %v, want %v", i, record.At, want.At)
				}
			}

			// A later round continues the sequence where the chunked one stopped.
			first, last, err = store.Save(ctx, checkpoint("task-1", 2), []agent.EventRecord{event("message", `"next"`)})
			if err != nil {
				t.Fatalf("Save 2: %v", err)
			}
			if first != int64(size)+1 || last != int64(size)+1 {
				t.Fatalf("range = %d..%d, want %d", first, last, size+1)
			}
		})
	}
}

// An event the producer left undated takes the save's instant, and one round's
// undated events share it however many INSERT statements carried them.
func TestSaveStampsUndatedEventsWithOneInstant(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	events := make([]agent.EventRecord, 2*eventInsertRows+1)
	for i := range events {
		events[i] = event("message", `"undated"`)
	}
	before := time.Now()
	if _, _, err := store.Save(ctx, checkpoint("task-1", 1), events); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after := time.Now()

	stored, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(stored) != len(events) {
		t.Fatalf("events = %d, want %d", len(stored), len(events))
	}
	instant := stored[0].At
	if instant.IsZero() {
		t.Fatal("an undated event read back as unknown")
	}
	if instant.Before(before) || instant.After(after) {
		t.Errorf("at = %v, want the save's instant in [%v, %v]", instant, before, after)
	}
	for i, record := range stored {
		if !record.At.Equal(instant) {
			t.Errorf("event %d at = %v, want the round's single instant %v", i, record.At, instant)
		}
	}
}

// BenchmarkSaveManyEvents is a step that writes several facts at once: the cost
// must be the commit, not one statement per fact.
func BenchmarkSaveManyEvents(b *testing.B) {
	body, err := json.Marshal(message.NewTextMessage(message.RoleUser, "one turn"))
	if err != nil {
		b.Fatalf("encode: %v", err)
	}
	events := make([]agent.EventRecord, 32)
	for i := range events {
		events[i] = agent.EventRecord{Kind: "message", Payload: body}
	}
	for _, mode := range syncModes {
		b.Run(mode.name, func(b *testing.B) {
			store := benchStore(b, mode.opts...)
			ctx := context.Background()
			checkpoint := agent.Checkpoint{
				Schema: agent.CheckpointSchema,
				Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateWorking},
			}
			if _, _, err := store.Save(ctx, checkpoint, events[:0]); err != nil {
				b.Fatalf("seed save: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, _, err := store.Save(ctx, checkpoint, events); err != nil {
					b.Fatalf("save: %v", err)
				}
			}
		})
	}
}

// BenchmarkLoadProjection measures what a restore pays for the projection: it
// must not grow with the event log, which is what EventsAfter is for.
func BenchmarkLoadProjection(b *testing.B) {
	for _, size := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("events=%d", size), func(b *testing.B) {
			store := benchStore(b)
			ctx := context.Background()
			checkpoint := agent.Checkpoint{
				Schema: agent.CheckpointSchema,
				Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateWorking},
			}
			events := make([]agent.EventRecord, 0, size)
			for i := 0; i < size; i++ {
				body, err := json.Marshal(message.NewTextMessage(message.RoleUser, "one turn"))
				if err != nil {
					b.Fatalf("encode: %v", err)
				}
				events = append(events, agent.EventRecord{Kind: "message", Payload: body})
			}
			if _, _, err := store.Save(ctx, checkpoint, events); err != nil {
				b.Fatalf("seed save: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				loaded, err := store.Load(ctx, "t1")
				if err != nil {
					b.Fatalf("load: %v", err)
				}
				if len(loaded.Task.History) != 0 {
					b.Fatalf("history = %d, want the projection only", len(loaded.Task.History))
				}
			}
			b.StopTimer()
			// The suffix is what a restore actually reads once a snapshot covers
			// the prefix; the range query must not scan the whole log.
			events, err := store.Events(ctx, "t1")
			if err != nil || len(events) != size {
				b.Fatalf("events = %d, want %d", len(events), size)
			}
			from := events[len(events)-10].Seq
			b.StartTimer()
			for b.Loop() {
				suffix, err := store.EventsAfter(ctx, "t1", from)
				if err != nil {
					b.Fatalf("events after: %v", err)
				}
				if len(suffix) != 10 {
					b.Fatalf("suffix = %d, want 10", len(suffix))
				}
			}
		})
	}
}
