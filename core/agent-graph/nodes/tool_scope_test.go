package nodes_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// soleRecorder is the run's projection when the run carries a recorder that is
// not a catalog view. The node cannot narrow it, so it builds a view of its own;
// the scope still has to survive the batch.
type soleRecorder struct{}

func (soleRecorder) RecordCall(message.ToolCall) {}

// drainNodeWith pulls a node's stream with the caller's context, which is what
// the graph engine does: the node's worker goroutines derive from it, and a
// scope attached by the run is visible to every call.
func drainNodeWith(t testing.TB, ctx context.Context, stream graph.NodeStream) (graph.Result, []agent.Event) {
	t.Helper()
	var events []agent.Event
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if event != nil {
			events = append(events, event)
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return result, events
}

// TestToolBatchKeepsTheRunScope pins that a tool call never shadows the
// projection its run attached: the node builds a view because the run's recorder
// is not one, but the tool still sees the run's recorder rather than the view.
func TestToolBatchKeepsTheRunScope(t *testing.T) {
	var mu sync.Mutex
	var seen []tool.CallRecorder
	answer := tool.TextTool(
		message.ToolDefinition{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, _ string) (string, error) {
			scope, ok := tool.ScopeFrom(ctx)
			if !ok || scope.Recorder == nil {
				return "", errors.New("the tool call carried no scope")
			}
			mu.Lock()
			seen = append(seen, scope.Recorder)
			mu.Unlock()
			return "ok", nil
		},
	)
	rt := newStubRuntime(t, &stubProvider{}, answer)
	run := soleRecorder{}
	ctx := tool.WithScope(context.Background(), tool.Scope{Recorder: run})

	calls, invoke := pendingCalls(t, 3, `{"messages":"messages","pending":"pending"}`)
	stream, err := nodes.Tool(rt).Invoke(ctx, invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNodeWith(t, ctx, stream)
	requireCallOrder(t, calls, toolTurnCallIDs(t, result))

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(calls) {
		t.Fatalf("tools saw %d scopes, want one per call (%d)", len(seen), len(calls))
	}
	for _, recorder := range seen {
		if recorder != tool.CallRecorder(run) {
			t.Fatal("the tool call shadowed the run's scope")
		}
	}
}
