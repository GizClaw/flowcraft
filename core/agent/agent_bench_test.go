package agent_test

import (
	"context"
	"iter"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// benchRunner is the cheapest engine worth measuring against: it emits a fixed
// script and allocates nothing.
type benchRunner struct{ events int }

func (b *benchRunner) State() agent.TaskState { return agent.StateCompleted }

func (b *benchRunner) Run(_ context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		for i := 0; i < b.events; i++ {
			if !yield(agent.Text{Text: "x"}, nil) {
				return
			}
		}
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (b *benchRunner) Cancel(context.Context, string) error { return nil }
func (b *benchRunner) Close() error                         { return nil }

func newBenchAgent(b *testing.B, events int) *agent.Agent {
	b.Helper()
	deployed, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return &benchRunner{events: events}, nil
		},
	})
	if err != nil {
		b.Fatalf("NewAgent: %v", err)
	}
	return deployed
}

var benchMessage = message.NewTextMessage(message.RoleUser, "hello")

// BenchmarkAgentRun measures one message end to end with a three-event answer.
// Each iteration rolls the finished task over to the next, so it includes the
// table lookup, the slot, the identity minting and the runner rebuild.
func BenchmarkAgentRun(b *testing.B) {
	deployed := newBenchAgent(b, 1)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for event, err := range deployed.Run(ctx, "c1", benchMessage) {
			if err != nil {
				b.Fatalf("run: %v", err)
			}
			_ = event
		}
	}
}

// BenchmarkAgentRunEvents measures the per-event dispatch cost: one fixed cost
// plus the cost of forwarding a stream.
func BenchmarkAgentRunEvents(b *testing.B) {
	deployed := newBenchAgent(b, 100)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for event, err := range deployed.Run(ctx, "c1", benchMessage) {
			if err != nil {
				b.Fatalf("run: %v", err)
			}
			_ = event
		}
	}
}

// BenchmarkAgentRunParallel measures the table under concurrent runs on
// distinct sessions, which is what a server actually does.
func BenchmarkAgentRunParallel(b *testing.B) {
	deployed := newBenchAgent(b, 1)
	var next atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		id := "c" + strconv.FormatInt(next.Add(1), 10)
		ctx := context.Background()
		for pb.Next() {
			for event, err := range deployed.Run(ctx, id, benchMessage) {
				if err != nil {
					b.Fatalf("run: %v", err)
				}
				_ = event
			}
		}
	})
}

// BenchmarkAgentRunBusy measures the refusal path: a second message while the
// session's run is in flight.
func BenchmarkAgentRunBusy(b *testing.B) {
	deployed := make(chan struct{})
	release := make(chan struct{})
	blocked, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return &blockingRunner{entered: deployed, release: release}, nil
		},
	})
	if err != nil {
		b.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range blocked.Run(ctx, "c1", benchMessage) {
		}
	}()
	<-deployed
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, err := range blocked.Run(ctx, "c1", benchMessage) {
			if err == nil {
				b.Fatalf("second run was accepted")
			}
		}
	}
	b.StopTimer()
	close(release)
	<-done
}

// blockingRunner holds its run open until released, so the busy path has
// something to collide with.
type blockingRunner struct {
	entered chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (b *blockingRunner) State() agent.TaskState { return agent.StateWorking }
func (b *blockingRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if b.once.CompareAndSwap(false, true) {
			close(b.entered)
		}
		yield(agent.Status{State: agent.StateWorking}, nil)
		<-b.release
	}
}
func (b *blockingRunner) Cancel(context.Context, string) error { return nil }
func (b *blockingRunner) Close() error                         { return nil }

// BenchmarkAgentStateOpen measures a lookup on a session this process has
// already served: the hot path for a protocol adapter.
func BenchmarkAgentStateOpen(b *testing.B) {
	deployed := newBenchAgent(b, 1)
	ctx := context.Background()
	for range deployed.Run(ctx, "c1", benchMessage) {
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := deployed.State(ctx, "c1"); err != nil {
			b.Fatalf("state: %v", err)
		}
	}
}

// BenchmarkAgentStateStored measures the cold lookup: no session in memory,
// one store read.
func BenchmarkAgentStateStored(b *testing.B) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema:  agent.CheckpointSchema,
		Runtime: "sha256:one",
		Task:    agent.Task{ID: "t1", SessionID: "c1", State: agent.StateCompleted},
	}, nil); err != nil {
		b.Fatalf("Save: %v", err)
	}
	deployed, err := agent.NewAgent(agent.AgentOptions{
		Store:     store,
		Identity:  "sha256:one",
		NewRunner: func(context.Context, string, string) (agent.Runner, error) { return &benchRunner{}, nil },
	})
	if err != nil {
		b.Fatalf("NewAgent: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := deployed.State(ctx, "c1"); err != nil {
			b.Fatalf("state: %v", err)
		}
	}
}
