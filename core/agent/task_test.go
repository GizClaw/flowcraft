package agent_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Terminal is the one predicate every consumer branches on, so its table is
// worth pinning: a state that becomes terminal by accident strands a
// conversation, and one that stops being terminal makes a finished task take
// messages.
func TestTaskStateTerminal(t *testing.T) {
	terminal := []agent.TaskState{
		agent.StateCompleted,
		agent.StateFailed,
		agent.StateCanceled,
		agent.StateRejected,
	}
	for _, state := range terminal {
		if !state.Terminal() {
			t.Errorf("%s is not terminal", state)
		}
	}
	alive := []agent.TaskState{
		agent.StateSubmitted,
		agent.StateWorking,
		agent.StateInputRequired,
		agent.StateAuthRequired,
	}
	for _, state := range alive {
		if state.Terminal() {
			t.Errorf("%s is terminal", state)
		}
	}
}

// A Task is what a checkpoint stores, so its JSON must round-trip every field —
// including the opaque tool-call arguments, which is where a value receiver
// silently dropped the bytes once.
func TestTaskJSONRoundTrip(t *testing.T) {
	call, err := message.NewToolCall("call-1", "read_file", map[string]string{"path": "notes.txt"})
	if err != nil {
		t.Fatalf("NewToolCall: %v", err)
	}
	task := agent.Task{
		ID:        "t1",
		ContextID: "c1",
		State:     agent.StateInputRequired,
		Round:     3,
		History: []message.Message{
			message.NewTextMessage(message.RoleUser, "read it"),
			message.NewTextMessage(message.RoleAssistant, "which one?"),
		},
		Artifacts: []agent.Artifact{{
			ID:        "a1",
			Name:      "notes",
			MediaType: "text/plain",
			Blob:      "blob:sha256:1",
		}},
		Pending: &call,
	}
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back agent.Task
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(task, back) {
		t.Fatalf("round trip changed the task:\nwant %+v\ngot  %+v", task, back)
	}
	if back.Pending == nil {
		t.Fatal("the pending call was dropped")
	}
	if got := back.Pending.Arguments.String(); got != `{"path":"notes.txt"}` {
		t.Errorf("arguments = %s, want the object", got)
	}
}
