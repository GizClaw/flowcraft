package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
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
