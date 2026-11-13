package checkpoint

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// syncModes is the durability choice a host makes, named for sub-benchmarks.
var syncModes = []struct {
	name string
	opts []Option
}{
	{"full", nil},
	{"normal", []Option{WithSynchronous(SynchronousNormal)}},
}

// BenchmarkSetServerVersion is the task store's fast path: a non-terminal event
// with no message advances only the adapter's counter, never the projection.
func BenchmarkSetServerVersion(b *testing.B) {
	for _, mode := range syncModes {
		b.Run(mode.name, func(b *testing.B) {
			store := benchStore(b, mode.opts...)
			ctx := context.Background()
			if _, _, err := store.Save(ctx, agent.Checkpoint{
				Schema: agent.CheckpointSchema,
				Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateWorking},
			}, nil); err != nil {
				b.Fatalf("seed save: %v", err)
			}
			version := int64(0)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				version++
				if err := store.SetServerVersion(ctx, "t1", version); err != nil {
					b.Fatalf("set server version: %v", err)
				}
			}
		})
	}
}

// BenchmarkTruncateEvents is compaction at the frequency production uses it: a
// base is written every `every` steps, and the events the base covers are then
// dropped. The per-op number is one batch; divide by `every` for the cost each
// step pays.
//
// separate is the old shape (append, then truncate as a second call); folded is
// the same decision inside the save's transaction.
func BenchmarkTruncateEvents(b *testing.B) {
	shapes := []struct {
		name string
		fold bool
	}{
		{"separate", false},
		{"folded", true},
	}
	for _, every := range []int{1, 10, 100} {
		for _, mode := range syncModes {
			for _, shape := range shapes {
				name := fmt.Sprintf("every=%d/sync=%s/%s", every, mode.name, shape.name)
				b.Run(name, func(b *testing.B) {
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
					batch := make([]agent.EventRecord, every)
					for i := range batch {
						batch[i] = agent.EventRecord{Kind: "message", Payload: body}
					}
					if _, _, err := store.Save(ctx, checkpoint, batch[:1]); err != nil {
						b.Fatalf("seed save: %v", err)
					}
					// through is the last sequence the current base covers; it is
					// known before the save, which is what lets the two become one
					// transaction.
					through := int64(1)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if shape.fold {
							if _, _, err := store.Save(ctx, checkpoint, batch,
								agent.TruncateThrough(through)); err != nil {
								b.Fatalf("folded save: %v", err)
							}
						} else {
							if _, _, err := store.Save(ctx, checkpoint, batch); err != nil {
								b.Fatalf("append: %v", err)
							}
							if err := store.TruncateEvents(ctx, "t1", through); err != nil {
								b.Fatalf("truncate: %v", err)
							}
						}
						through += int64(every)
					}
				})
			}
		}
	}
}
