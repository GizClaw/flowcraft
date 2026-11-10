// Edge conditions: the environment they read and what a value projects to.
package graph

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// The expression environment namespaces state and exposes the step counter.
func TestEnvNamespacesStateAndStep(t *testing.T) {
	env := Env(3, NewSnapshot(map[string]Value{"route": Text("x")}))
	if got := env["step"]; got != 3 {
		t.Errorf("step = %v, want 3", got)
	}
	state, ok := env["state"].(map[string]any)
	if !ok {
		t.Fatalf("state = %T, want a map", env["state"])
	}
	if state["route"] != "x" {
		t.Errorf("state.route = %v", state["route"])
	}
	if _, bare := env["route"]; bare {
		t.Error("a state key leaked to the top level of the environment")
	}
}

// An edge condition can read the step counter.
func TestConditionCanReadTheStep(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: start
state:
  seed: {type: text}
nodes:
  - {id: start, kind: const, config: {key: seed, value: x}}
  - {id: later, kind: count}
edges:
  - {from: start, to: later, when: 'step < 1'}
  - {from: later, to: end}
output: {key: seed}
`, counter)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
	if counter.count("later") != 1 {
		t.Errorf("later ran %d times, want 1", counter.count("later"))
	}
}

// Plain projects a session and a media value for expressions.
func TestPlainProjectsMessagesAndMedia(t *testing.T) {
	messages := MessagesFromSlice([]message.Message{
		message.NewTextMessage(message.RoleUser, "hi"),
	})
	projected, ok := Plain(MessagesValue(messages)).([]message.Message)
	if !ok || len(projected) != 1 {
		t.Fatalf("messages projection = %T (%v)", Plain(MessagesValue(messages)), len(projected))
	}
	media, ok := Plain(Image("image/png", []byte("x"))).(map[string]any)
	if !ok {
		t.Fatalf("media projection = %T", Plain(Image("image/png", []byte("x"))))
	}
	if media["media_type"] != "image/png" {
		t.Errorf("media_type = %v", media["media_type"])
	}
}
