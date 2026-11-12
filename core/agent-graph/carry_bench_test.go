package graph

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

func drainBench(b *testing.B, run *Run) {
	b.Helper()
	for _, err := range run.Events(context.Background()) {
		if err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// BenchmarkCarryStart is the cost a turn pays to continue its session: the store
// lookup, the replay of the previous task's state, and the copy of the carried
// keys. The baseline is the same graph in a fresh session, where nothing is
// carried.
func BenchmarkCarryStart(b *testing.B) {
	store := agenttest.NewMemStore()
	g := benchmarkGraph(b, carryDocument, lengthKind{})
	ctx := context.Background()

	// One previous task holding a transcript, which is what a chat session carries.
	const size = 200
	seed := make([]message.Message, 0, size)
	for i := 0; i < size; i++ {
		role := message.RoleUser
		if i%2 == 1 {
			role = message.RoleAssistant
		}
		seed = append(seed, message.NewTextMessage(role, "这是会话里的一条消息，用来把被携带的历史撑到真实大小。"))
	}
	first, err := g.Start(ctx, seed, WithStore(store, "seed"), WithSession("conv-1"))
	if err != nil {
		b.Fatalf("seed start: %v", err)
	}
	drainBench(b, first)

	turn := []message.Message{message.NewTextMessage(message.RoleUser, "这一轮的新问题")}

	b.Run("fresh session", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			run, err := g.Start(ctx, turn,
				WithStore(store, fmt.Sprintf("fresh-%d", i)),
				WithSession(fmt.Sprintf("fresh-conv-%d", i)))
			if err != nil {
				b.Fatalf("start: %v", err)
			}
			drainBench(b, run)
		}
	})

	// The carried loop above advances the session every iteration, so its transcript
	// grows; this one replays the same 200-message task, which is the steady-state
	// cost of one carry.
	b.Run("carry only, fixed transcript", func(b *testing.B) {
		b.ReportAllocs()
		previous := "seed"
		for i := 0; i < b.N; i++ {
			target := newStateStore(g.state)
			if err := g.carryInto(ctx, runOptions{
				store: store, sessionID: "conv-1", taskID: fmt.Sprintf("nobody-%d", i),
			}, target); err != nil {
				b.Fatalf("carry: %v", err)
			}
			if previous == "" {
				b.Fatal("unreachable")
			}
		}
	})

	b.Run("carried transcript", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			run, err := g.Start(ctx, turn,
				WithStore(store, fmt.Sprintf("carry-%d", i)),
				WithSession("conv-1"))
			if err != nil {
				b.Fatalf("start: %v", err)
			}
			drainBench(b, run)
		}
	})
}
