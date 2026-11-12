package script_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/starlark"
	"github.com/GizClaw/flowcraft/core/message"
)

// messagesKind builds the node over a source that reads a transcript. It is the
// path a chat-shaped rule takes, and the one the projection optimisation touched.
func messagesKind(t *testing.T, source string) graph.NodeKind {
	t.Helper()
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithSource(staticSource{"rule.star": source}),
		script.WithLog(io.Discard),
	)
	if err != nil {
		t.Fatalf("script.New: %v", err)
	}
	return kind
}

func history() graph.Value {
	return graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
		message.NewTextMessage(message.RoleUser, "第一句"),
		message.NewTextMessage(message.RoleAssistant, "第二句"),
		message.NewTextMessage(message.RoleUser, "第三句"),
	}))
}

// runTranscript invokes one rule against a transcript and returns the answer it
// wrote.
func runTranscript(t *testing.T, kind graph.NodeKind, source graph.Value) (string, error) {
	t.Helper()
	values := map[string]graph.Value{
		"history": source,
		"answer":  graph.Text(""),
	}
	in := graph.Invoke{
		Node:     "rule",
		Config:   json.RawMessage(`{"backend":"starlark","path":"rule.star","reads":["history"],"writes":["answer"]}`),
		State:    func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(values) },
	}
	stream, err := kind.Invoke(context.Background(), in)
	if err != nil {
		return "", err
	}
	var result graph.Result
	for {
		if _, err := stream.Next(context.Background()); err != nil {
			if !errors.Is(err, io.EOF) {
				return "", err
			}
			break
		}
	}
	if result, err = stream.Result(); err != nil {
		return "", err
	}
	for _, update := range result.State {
		if update.Key == "answer" {
			text, _ := update.Value.Text()
			return text, nil
		}
	}
	return "", errors.New("the rule wrote no answer")
}

func TestScriptReadsTheTranscriptAsDicts(t *testing.T) {
	kind := messagesKind(t, `
def run(state):
    return {"answer": state["history"][-1]["text"]}
`)
	answer, err := runTranscript(t, kind, history())
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if answer != "第三句" {
		t.Fatalf("answer = %q, want the last message", answer)
	}
}

func TestScriptSeesRolesAndLength(t *testing.T) {
	kind := messagesKind(t, `
def run(state):
    roles = [message["role"] for message in state["history"]]
    return {"answer": str(len(roles)) + ":" + ",".join(roles)}
`)
	answer, err := runTranscript(t, kind, history())
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if answer != "3:user,assistant,user" {
		t.Fatalf("answer = %q", answer)
	}
}

// TestScriptCannotPoisonTheNextProjection pins the reason the projection is
// rebuilt per invocation instead of cached: the list a script receives is a fresh
// value, so a rule that mutates it cannot change what the next step reads.
func TestScriptCannotPoisonTheNextProjection(t *testing.T) {
	kind := messagesKind(t, `
def run(state):
    count = len(state["history"])
    for i in range(count):
        state["history"].pop()
    return {"answer": str(count)}
`)
	source := history()
	for round := 1; round <= 2; round++ {
		answer, err := runTranscript(t, kind, source)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if answer != "3" {
			t.Fatalf("round %d saw %s messages, want the full transcript", round, answer)
		}
	}
}
