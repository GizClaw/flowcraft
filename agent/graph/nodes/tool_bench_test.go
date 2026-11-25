package nodes_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/agent/graph/internal/graphtest"
	"github.com/GizClaw/flowcraft/agent/graph/nodes"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// benchEchoDefinition names the fixture's one tool.
func benchEchoDefinition() message.ToolDefinition {
	return message.ToolDefinition{
		Name:        "echo",
		Description: "answers with a fixed string",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func benchToolRuntime(tb testing.TB, answer tool.Tool) *graphtest.Fixture {
	tb.Helper()
	return graphtest.New(tb, graphtest.Config{
		Generator: &stubProvider{},
		Tools:     []tool.Tool{answer},
	})
}

// benchEchoRuntime answers instantly: the fixed cost of a call is the node's
// machinery, not the tool behind it.
func benchEchoRuntime(tb testing.TB) *graphtest.Fixture {
	tb.Helper()
	return benchToolRuntime(tb, tool.FuncTool(benchEchoDefinition(),
		func(context.Context, string) (message.Content, error) {
			return message.NewTextContent("ok"), nil
		}))
}

// concurrencyGauge counts the tool executions in flight and the largest number
// that ever overlapped. The atomic is the whole point: the workers run in the
// node, so only the tool can observe how many of them reach it at once.
type concurrencyGauge struct {
	inFlight atomic.Int64
	peak     atomic.Int64
	pause    time.Duration
}

// enter marks one execution running and raises the recorded peak if this one
// makes history.
func (g *concurrencyGauge) enter() {
	running := g.inFlight.Add(1)
	for {
		seen := g.peak.Load()
		if running <= seen || g.peak.CompareAndSwap(seen, running) {
			return
		}
	}
}

func (g *concurrencyGauge) tool() tool.Tool {
	return tool.FuncTool(benchEchoDefinition(), func(ctx context.Context, _ string) (message.Content, error) {
		g.enter()
		defer g.inFlight.Add(-1)
		// A pause wider than the scheduler tick, so a batch that could overlap
		// actually does and the peak means something.
		select {
		case <-time.After(g.pause):
		case <-ctx.Done():
			return message.Content{}, ctx.Err()
		}
		return message.NewTextContent("ok"), nil
	})
}

// pendingCalls builds the batch and the state the node reads it from.
func pendingCalls(tb testing.TB, rt *graphtest.Fixture, calls int, config string) ([]message.ToolCall, graph.Invoke) {
	tb.Helper()
	pending := make([]message.ToolCall, calls)
	for i := range pending {
		call, err := message.NewToolCall(fmt.Sprintf("c-%d", i), "echo", map[string]any{"n": i})
		if err != nil {
			tb.Fatalf("NewToolCall: %v", err)
		}
		pending[i] = call
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		tb.Fatalf("marshal pending: %v", err)
	}
	state := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
		"pending": graph.JSON(encoded),
	}
	invoke := graph.Invoke{
		Node:     "tools",
		Config:   json.RawMessage(config),
		State:    func(key string) graph.Value { return state[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(state) },
	}
	rt.Stamp(&invoke)
	return pending, invoke
}

// toolTurnCallIDs reads the call ids of the one tool turn the node appended, in
// transcript order.
func toolTurnCallIDs(tb testing.TB, result graph.Result) []string {
	tb.Helper()
	var turns graph.Messages
	for _, update := range result.State {
		if update.Key != "messages" {
			continue
		}
		messages, err := update.Value.Messages()
		if err != nil {
			tb.Fatalf("messages: %v", err)
		}
		turns = messages
	}
	if len(turns.Slice()) != 1 {
		tb.Fatalf("transcript delta = %d messages, want 1", len(turns.Slice()))
	}
	parts := turns.Slice()[0].Content.Parts
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		typed, ok := part.(message.ToolResultPart)
		if !ok {
			tb.Fatalf("the tool turn holds a %T, want tool results only", part)
		}
		ids = append(ids, typed.Result.CallID)
	}
	return ids
}

// requireCallOrder checks the transcript kept the batch's input order.
func requireCallOrder(tb testing.TB, calls []message.ToolCall, got []string) {
	tb.Helper()
	if len(got) != len(calls) {
		tb.Fatalf("results = %d, want one per call (%d)", len(got), len(calls))
	}
	for i, call := range calls {
		if got[i] != call.ID {
			tb.Fatalf("result %d = %q, want %q: the transcript left input order", i, got[i], call.ID)
		}
	}
}

// BenchmarkToolNodeCalls separates the node's fixed cost from its per-call cost.
// The pool sizes itself to the batch, so calls=1 pays for the same machinery as
// calls=4 and calls=64 — the last case caps the pool, which is where a batch
// stops costing a goroutine per call.
func BenchmarkToolNodeCalls(b *testing.B) {
	for _, batch := range []struct {
		calls       int
		parallelism int
		pause       time.Duration
	}{
		{calls: 1},
		{calls: 4},
		// A cap only shows itself when the batch can exceed it.
		{calls: 64, parallelism: 4, pause: 100 * time.Microsecond},
	} {
		b.Run(fmt.Sprintf("calls=%d", batch.calls), func(b *testing.B) {
			gauge := &concurrencyGauge{pause: batch.pause}
			rt := benchToolRuntime(b, gauge.tool())
			kind := nodes.Tool()
			config := `{"messages":"messages","pending":"pending"}`
			if batch.parallelism > 0 {
				config = fmt.Sprintf(`{"messages":"messages","pending":"pending","parallelism":%d}`,
					batch.parallelism)
			}
			calls, invoke := pendingCalls(b, rt, batch.calls, config)
			// The run context carries the catalog view, attached once per run by
			// host.Session; a benchmark without it would pay a view construction
			// per invocation that a real node invocation never pays.
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				stream, err := kind.Invoke(ctx, invoke)
				if err != nil {
					b.Fatalf("Invoke: %v", err)
				}
				result, _ := drainNode(b, stream)
				requireCallOrder(b, calls, toolTurnCallIDs(b, result))
				if batch.parallelism > 0 {
					if peak := gauge.peak.Load(); peak > int64(batch.parallelism) {
						b.Fatalf("%d calls ran at once, want at most %d", peak, batch.parallelism)
					}
				}
			}
		})
	}
}

// TestToolConcurrencyIsCapped is the asserted half of the pool: a batch of 64
// calls capped at 4 never has more than 4 executions in flight, and the results
// still land in input order.
func TestToolConcurrencyIsCapped(t *testing.T) {
	const (
		calls       = 64
		parallelism = 4
	)
	gauge := &concurrencyGauge{pause: 5 * time.Millisecond}
	rt := benchToolRuntime(t, gauge.tool())
	batch, invoke := pendingCalls(t, rt, calls, fmt.Sprintf(
		`{"messages":"messages","pending":"pending","parallelism":%d}`, parallelism))
	ctx := context.Background()

	stream, err := nodes.Tool().Invoke(ctx, invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, stream)

	if peak := gauge.peak.Load(); peak > parallelism {
		t.Fatalf("peak concurrency = %d, want at most %d", peak, parallelism)
	}
	requireCallOrder(t, batch, toolTurnCallIDs(t, result))
	if cleared, err := updateFor(t, result, "pending").JSON(); err != nil || string(cleared) != "[]" {
		t.Fatalf("pending after execution = %s (%v), want []", cleared, err)
	}
}

// TestToolSpawnsOneWorkerPerSlot is the goroutine half of the pool: a batch of
// 64 calls capped at 4 keeps 4 workers and one dispatcher blocked in the node,
// not 64 goroutines parked on a semaphore.
func TestToolSpawnsOneWorkerPerSlot(t *testing.T) {
	const (
		calls       = 64
		parallelism = 4
	)
	entered := make(chan struct{}, calls)
	release := make(chan struct{})
	blocking := tool.FuncTool(benchEchoDefinition(),
		func(ctx context.Context, _ string) (message.Content, error) {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return message.Content{}, ctx.Err()
			}
			return message.NewTextContent("ok"), nil
		})
	rt := benchToolRuntime(t, blocking)
	batch, invoke := pendingCalls(t, rt, calls, fmt.Sprintf(
		`{"messages":"messages","pending":"pending","parallelism":%d}`, parallelism))
	ctx := context.Background()
	before := goruntime.NumGoroutine()

	stream, err := nodes.Tool().Invoke(ctx, invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for i := range calls {
		if _, err := stream.Next(ctx); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	tailed := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Next(ctx); err != nil {
				tailed <- err
				return
			}
		}
	}()
	// Every slot occupied and parked: the count below is the pool at full width.
	for range parallelism {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the pool did not fill its slots")
		}
	}
	// Workers, the dispatcher, and this test's own drainer; a goroutine per call
	// would be an order of magnitude above this.
	if during := goruntime.NumGoroutine(); during > before+parallelism+3 {
		t.Fatalf("goroutines went from %d to %d for %d blocked calls at parallelism %d",
			before, during, calls, parallelism)
	}
	close(release)

	select {
	case err := <-tailed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream error = %v, want io.EOF", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the batch did not finish after the tools were released")
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	requireCallOrder(t, batch, toolTurnCallIDs(t, result))
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestToolReportsStartsBeforeResults pins the event contract the pool had to
// keep: every start first, in input order, then one result per call.
func TestToolReportsStartsBeforeResults(t *testing.T) {
	for _, calls := range []int{1, 4} {
		t.Run(fmt.Sprintf("calls=%d", calls), func(t *testing.T) {
			rt := benchEchoRuntime(t)
			batch, invoke := pendingCalls(t, rt, calls, `{"messages":"messages","pending":"pending"}`)
			ctx := context.Background()

			stream, err := nodes.Tool().Invoke(ctx, invoke)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			result, events := drainNode(t, stream)

			if len(events) != 2*calls {
				t.Fatalf("events = %d, want %d (a start and a result per call)", len(events), 2*calls)
			}
			for i, call := range batch {
				start, ok := events[i].(agent.ToolCallStart)
				if !ok {
					t.Fatalf("event %d is a %T, want every start first", i, events[i])
				}
				if start.Call.ID != call.ID {
					t.Fatalf("start %d = %q, want %q: starts left input order", i, start.Call.ID, call.ID)
				}
			}
			seen := map[string]bool{}
			for i := calls; i < len(events); i++ {
				done, ok := events[i].(agent.ToolResult)
				if !ok {
					t.Fatalf("event %d is a %T, want a result", i, events[i])
				}
				if seen[done.Result.CallID] {
					t.Fatalf("result %q reported twice", done.Result.CallID)
				}
				seen[done.Result.CallID] = true
			}
			requireCallOrder(t, batch, toolTurnCallIDs(t, result))
		})
	}
}

// TestToolCancelStopsTheWorkers: a cancel mid-batch ends the stream with the
// context error and leaves no worker behind. The pool is what makes the second
// half checkable — before it, a canceled batch's goroutines were parked on a
// semaphore nobody released.
func TestToolCancelStopsTheWorkers(t *testing.T) {
	const calls = 8
	started := make(chan struct{}, calls)
	blocking := tool.FuncTool(benchEchoDefinition(),
		func(ctx context.Context, _ string) (message.Content, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return message.Content{}, ctx.Err()
		})
	rt := benchToolRuntime(t, blocking)
	_, invoke := pendingCalls(t, rt, calls, `{"messages":"messages","pending":"pending","parallelism":2}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Started before the node runs: the router and driver goroutines the runtime
	// keeps are not the batch's, and the comparison is about the batch's.
	before := goruntime.NumGoroutine()

	stream, err := nodes.Tool().Invoke(ctx, invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for i := range calls {
		if _, err := stream.Next(ctx); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	tailed := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Next(ctx); err != nil {
				tailed <- err
				return
			}
		}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no tool call reached the tool: the pool never dispatched")
	}
	cancel()

	select {
	case err := <-tailed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not end after the cancel")
	}
	if _, err := stream.Result(); err == nil {
		t.Fatal("a canceled stream reported a result")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The count is inherently racy (the runtime retires goroutines on its own
	// schedule), so the check is directional with a bounded settle loop.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if after := goruntime.NumGoroutine(); after <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines went from %d to %d after the cancel",
				before, goruntime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
