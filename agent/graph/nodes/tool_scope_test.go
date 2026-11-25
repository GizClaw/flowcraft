package nodes_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/agent/graph/internal/graphtest"
	"github.com/GizClaw/flowcraft/agent/graph/nodes"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// drainNodeWith pulls a node's stream with the caller's context, which is what
// the graph engine does: the node's worker goroutines derive from it.
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

// bindingRecorder is the tool a batch calls: it keeps the binding each call ran under, which is the
// one thing a concurrency frontier can get wrong.
type bindingRecorder struct {
	mu   sync.Mutex
	seen []tool.Binding
}

func (b *bindingRecorder) Definition() message.ToolDefinition {
	return message.ToolDefinition{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (*bindingRecorder) Needs() []tool.Need { return nil }

func (b *bindingRecorder) Execute(ctx context.Context, binding tool.Binding, _ string) (message.Content, error) {
	if err := ctx.Err(); err != nil {
		return message.Content{}, err
	}
	b.mu.Lock()
	b.seen = append(b.seen, binding)
	b.mu.Unlock()
	return message.NewTextContent("ok"), nil
}

// TestToolBatchCarriesTheNodesBinding pins that every call in a batch runs under the binding the node
// was given: the grant, the projection and the capability pocket are the node's, not a fresh set.
//
// It replaces a test of the old design, which had to prove that a call never shadowed the projection
// its run attached to the context. There is nothing to attach any more — the binding is an argument —
// so the invariant that survives is the one this asserts.
func TestToolBatchCarriesTheNodesBinding(t *testing.T) {
	answer := &bindingRecorder{}
	rt := graphtest.New(t, graphtest.Config{Generator: &stubProvider{}, Tools: []tool.Tool{answer}})

	calls, invoke := pendingCalls(t, rt, 3, `{"messages":"messages","pending":"pending"}`)
	stream, err := nodes.Tool().Invoke(context.Background(), invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNodeWith(t, context.Background(), stream)
	requireCallOrder(t, calls, toolTurnCallIDs(t, result))

	want, _, _ := rt.Substrate()
	answer.mu.Lock()
	defer answer.mu.Unlock()
	if len(answer.seen) != len(calls) {
		t.Fatalf("tools saw %d bindings, want one per call (%d)", len(answer.seen), len(calls))
	}
	for _, binding := range answer.seen {
		// Dispatch partitions the session's scratch space per tool, so the session is the node's
		// wrapped in a prefix — its identity is what has to match.
		if binding.Session == nil || binding.Session.ID() != want.Session.ID() {
			t.Fatal("a call ran under another session than the node's")
		}
		if binding.View != want.View {
			t.Fatal("a call ran under another projection than the node's")
		}
		if binding.Grant.Digest() != want.Grant.Digest() {
			t.Fatal("a call ran under another grant than the node's")
		}
	}
}
