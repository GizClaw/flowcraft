package nodes_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/agent/graph/internal/graphtest"
	"github.com/GizClaw/flowcraft/agent/graph/nodes"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// askFirst is a dispatcher that answers one call with a question before anything runs.
type askFirst struct {
	question *message.Content
	ran      atomic.Int32
}

func (d *askFirst) Execute(_ context.Context, _ tool.Binding, call message.ToolCall) message.ToolResult {
	d.ran.Add(1)
	return message.NewToolResult(call.ID, message.NewTextContent("ran"))
}

func (d *askFirst) ExecuteAll(ctx context.Context, b tool.Binding, calls []message.ToolCall) []message.ToolResult {
	out := make([]message.ToolResult, len(calls))
	for i, call := range calls {
		out[i] = d.Execute(ctx, b, call)
	}
	return out
}

func (d *askFirst) Ask(_ context.Context, _ tool.Binding, _ message.ToolCall) (*message.Content, error) {
	return d.question, nil
}

// TestAQuestionSuspendsTheBatchBeforeAnythingRuns is the pre-flight's whole contract: the node stops
// on the question, no call is executed, and nothing is written — so the resume replays no side
// effect.
func TestAQuestionSuspendsTheBatchBeforeAnythingRuns(t *testing.T) {
	answer := &bindingRecorder{}
	rt := graphtest.New(t, graphtest.Config{Tools: []tool.Tool{answer}})
	_, invoke := pendingCalls(t, rt, 2, `{"messages":"messages","pending":"pending"}`)

	question := message.NewTextContent("may I run this?")
	asked := &askFirst{question: &question}
	invoke.Tools = asked

	stream, err := nodes.Tool().Invoke(context.Background(), invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, events := drainNodeWith(t, context.Background(), stream)
	if result.Suspend == nil {
		t.Fatal("the batch ran instead of asking")
	}
	if !strings.Contains(result.Suspend.Question.Text(), "may I run this?") {
		t.Fatalf("question = %q", result.Suspend.Question.Text())
	}
	if ran := asked.ran.Load(); ran != 0 {
		t.Fatalf("the dispatcher ran %d calls before the question was answered", ran)
	}
	if len(result.State) != 0 {
		t.Fatalf("a suspended step wrote state: %+v", result.State)
	}
	if len(events) != 0 {
		t.Fatalf("a suspended step emitted %d events, want none before the batch", len(events))
	}
}

// TestNoQuestionRunsTheBatch keeps the ordinary path: a dispatcher that implements the pre-flight but
// has nothing to ask changes nothing.
func TestNoQuestionRunsTheBatch(t *testing.T) {
	answer := &bindingRecorder{}
	rt := graphtest.New(t, graphtest.Config{Tools: []tool.Tool{answer}})
	_, invoke := pendingCalls(t, rt, 2, `{"messages":"messages","pending":"pending"}`)

	asked := &askFirst{}
	invoke.Tools = asked
	stream, err := nodes.Tool().Invoke(context.Background(), invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNodeWith(t, context.Background(), stream)
	if result.Suspend != nil {
		t.Fatal("a batch with no question was suspended")
	}
	if ran := asked.ran.Load(); ran != 2 {
		t.Fatalf("ran %d calls, want 2", ran)
	}
}

// A dispatcher that cannot ask is not asked: the pre-flight is optional, and a fake without it keeps
// the old behaviour.
func TestADispatcherWithoutAPreflightRunsTheBatch(t *testing.T) {
	answer := &bindingRecorder{}
	rt := graphtest.New(t, graphtest.Config{Tools: []tool.Tool{answer}})
	_, invoke := pendingCalls(t, rt, 1, `{"messages":"messages","pending":"pending"}`)
	if _, ok := invoke.Tools.(tool.AskDispatcher); ok {
		t.Fatal("the fixture's dispatcher gained a pre-flight")
	}
	stream, err := nodes.Tool().Invoke(context.Background(), invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNodeWith(t, context.Background(), stream)
	if result.Suspend != nil {
		t.Fatal("a dispatcher without a pre-flight suspended")
	}
}
