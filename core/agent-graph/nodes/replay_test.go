package nodes_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// A replay answers from the record: the provider is never called, and the
// transcript gets the recorded turn.
func TestModelReplayAnswersFromTheRecord(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	kind := nodes.Model(rt)
	transcript := graph.MessagesFromSlice([]message.Message{
		message.NewTextMessage(message.RoleUser, "hi"),
	})
	state := map[string]graph.Value{
		"messages": graph.MessagesValue(transcript),
		"text":     graph.Text(""),
	}
	response, err := json.Marshal(message.NewTextMessage(message.RoleAssistant, "from record"))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var lookedUp string
	invoke := graph.Invoke{
		Node:   "model",
		Config: json.RawMessage(`{"messages":"messages","output":"text"}`),
		State:  func(key string) graph.Value { return state[key] },
		Snapshot: func() graph.Snapshot {
			return graph.NewSnapshot(state)
		},
		Lookup: func(hash string) (agent.LLMCallRecord, bool) {
			lookedUp = hash
			return agent.LLMCallRecord{Response: response}, true
		},
		Replay: true,
	}
	stream, err := kind.Invoke(context.Background(), invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var text string
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if typed, ok := event.(agent.Text); ok {
			text += typed.Text
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if text != "from record" {
		t.Errorf("text = %q, want the recorded answer", text)
	}
	if lookedUp == "" {
		t.Error("the node did not ask for a record")
	}
	var output string
	for _, update := range result.State {
		if update.Key == "text" {
			output, _ = update.Value.Text()
		}
	}
	if output != "from record" {
		t.Errorf("output = %q, want the recorded answer", output)
	}
}

// A miss is an error: replay must not silently turn into a live call.
func TestModelReplayMissFails(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	kind := nodes.Model(rt)
	state := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
		"text": graph.Text(""),
	}
	_, err := kind.Invoke(context.Background(), graph.Invoke{
		Node:   "model",
		Config: json.RawMessage(`{"messages":"messages","output":"text"}`),
		State:  func(key string) graph.Value { return state[key] },
		Snapshot: func() graph.Snapshot {
			return graph.NewSnapshot(state)
		},
		Lookup: func(string) (agent.LLMCallRecord, bool) { return agent.LLMCallRecord{}, false },
		Replay: true,
	})
	if err == nil {
		t.Fatal("a replay miss was accepted")
	}
}
