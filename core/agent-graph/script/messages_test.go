package script_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
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

// TestTranscriptIsReadOnly pins the property a cached projection would have lost:
// the sequence a rule receives is a view, so there is nothing to mutate.
func TestTranscriptIsReadOnly(t *testing.T) {
	kind := messagesKind(t, `
def run(state):
    state["history"].pop()
    return {"answer": "mutated"}
`)
	if _, err := runTranscript(t, kind, history()); err == nil {
		t.Fatal("a rule must not be able to mutate the transcript")
	} else if !strings.Contains(err.Error(), "pop") {
		t.Fatalf("error %v does not name the missing method", err)
	}

	// The transcript a later invocation reads is unchanged.
	reader := messagesKind(t, `
def run(state):
    return {"answer": str(state["history"].count)}
`)
	answer, err := runTranscript(t, reader, history())
	if err != nil {
		t.Fatalf("read after the failed mutation: %v", err)
	}
	if answer != "3" {
		t.Fatalf("answer = %q, want the full transcript", answer)
	}
}

// TestTranscriptNarrowAccessors covers the O(1) questions a chat-shaped rule asks.
func TestTranscriptNarrowAccessors(t *testing.T) {
	kind := messagesKind(t, `
def run(state):
    history = state["history"]
    return {"answer": str(history.count) + ":" + history.first["text"] + ":" + history.last["text"]}
`)
	answer, err := runTranscript(t, kind, history())
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if answer != "3:第一句:第三句" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestTranscriptEmptyAccessorsAreNone(t *testing.T) {
	kind := messagesKind(t, `
def run(state):
    history = state["history"]
    if history.last == None and history.count == 0:
        return {"answer": "empty"}
    return {"answer": "full"}
`)
	empty := graph.MessagesValue(graph.MessagesFromSlice(nil))
	answer, err := runTranscript(t, kind, empty)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if answer != "empty" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestTranscriptSlicesAndIndices(t *testing.T) {
	kind := messagesKind(t, `
def run(state):
    tail = state["history"][-2:]
    head = state["history"][0]
    return {"answer": str(len(tail)) + ":" + tail[0]["text"] + ":" + head["text"]}
`)
	answer, err := runTranscript(t, kind, history())
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if answer != "2:第二句:第一句" {
		t.Fatalf("answer = %q", answer)
	}
}

// TestTranscriptRejectsAStringIndex pins that the view is a sequence, not a map: a
// keyed lookup is an error rather than a silent miss.
func TestTranscriptRejectsAStringIndex(t *testing.T) {
	kind := messagesKind(t, `
def run(state):
    return {"answer": state["history"]["text"]}
`)
	if _, err := runTranscript(t, kind, history()); err == nil {
		t.Fatal("a transcript is not keyed")
	} else if !strings.Contains(err.Error(), "index") {
		t.Fatalf("error %v does not describe the index failure", err)
	}
}
