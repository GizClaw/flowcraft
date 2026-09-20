package script

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/message"
)

// The steer acceptance path, end to end at the graph layer: a document
// places a script node at a round boundary (after the tool result,
// before the next round), the node drains host.drainSteer() and appends
// the messages to the main channel, and the next node — the next wave —
// sees them in its board. The message arrives as a legal role
// transition (a user message right after the tool result), which is
// exactly what makes the appended content readable by the next
// inference round.

// steerTestMessages builds the assistant tool call and its tool result
// that the steer text must land *after*.
func steerTestMessages(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	call, err := message.NewToolCall("call-1", "shell", map[string]any{"command": "go test ./..."})
	if err != nil {
		t.Fatalf("NewToolCall: %v", err)
	}
	assistant := message.Message{
		Role:    message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}},
	}
	tool := message.Message{
		Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.NewTextToolResult("call-1", "ok")},
		}},
	}
	toWire := func(msg message.Message) map[string]any {
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal %s message: %v", msg.Role, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal %s message: %v", msg.Role, err)
		}
		return out
	}
	return toWire(assistant), toWire(tool)
}

// wireRole reads the role of a wire message the board bindings produced.
func wireRole(raw any) string {
	msg, _ := raw.(map[string]any)
	role, _ := msg["role"].(string)
	return role
}

// wireText reads the text of a wire message carrying exactly one text
// part — the shape a steered message has when a script appends it.
func wireText(t *testing.T, raw any) string {
	t.Helper()
	msg, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("wire message = %T, want an object", raw)
	}
	content, ok := msg["content"].(map[string]any)
	if !ok {
		t.Fatalf("wire content = %T, want an object", msg["content"])
	}
	parts, ok := content["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("wire parts = %v, want exactly one", content["parts"])
	}
	part, ok := parts[0].(map[string]any)
	if !ok {
		t.Fatalf("wire part = %T, want an object", parts[0])
	}
	text, _ := part["text"].(string)
	return text
}

func TestScriptNode_SteerTextLandsAfterTheToolResult(t *testing.T) {
	const steerText = "actually, use the vendored copy"

	host := agenttest.NewMockHost()
	host.Steer(message.NewTextMessage(message.RoleUser, steerText))

	var (
		drainedText   string
		drainedAtTool bool
		nextStepSeen  bool
		nextStepRoles []string
	)
	rt := fakeRuntime{exec: func(_ context.Context, nodeID, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		board := env.Bindings["board"].(map[string]any)
		main := board["MAIN_CHANNEL"].(string)
		appendChannel := board["appendChannel"].(func(string, any) error)
		channel := board["channel"].(func(string) ([]any, error))
		drainSteer := env.Bindings["host"].(map[string]any)["drainSteer"].(func() ([]any, error))

		switch nodeID {
		case "run-tool":
			assistant, tool := steerTestMessages(t)
			if err := appendChannel(main, assistant); err != nil {
				return nil, err
			}
			if err := appendChannel(main, tool); err != nil {
				return nil, err
			}
		case "steer":
			// The boundary this node sits on: the round ended with the
			// tool result, the user's text has not been seen yet.
			before, err := channel(main)
			if err != nil {
				return nil, err
			}
			drainedAtTool = wireRole(before[len(before)-1]) == string(message.RoleTool)

			pending, err := drainSteer()
			if err != nil {
				return nil, err
			}
			if len(pending) != 1 {
				t.Errorf("drainSteer returned %d messages, want 1", len(pending))
			}
			for _, msg := range pending {
				if err := appendChannel(main, msg); err != nil {
					return nil, err
				}
			}
			// Draining is take-all: the queue is empty for the next
			// boundary.
			if again, err := drainSteer(); err != nil || len(again) != 0 {
				t.Errorf("second drainSteer = (%v, %v), want an empty array", again, err)
			}
		case "observe":
			msgs, err := channel(main)
			if err != nil {
				return nil, err
			}
			nextStepSeen = true
			for _, msg := range msgs {
				nextStepRoles = append(nextStepRoles, wireRole(msg))
			}
			drainedText = wireText(t, msgs[len(msgs)-1])
		}
		return nil, nil
	}}

	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	rawConfig := func(source string) json.RawMessage {
		raw, err := json.Marshal(ScriptConfig{Runtime: "fake", Source: source})
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		return raw
	}
	g, err := graph.Build(&graph.GraphDefinition{
		Name:  "steer-graph",
		Entry: "run-tool",
		Nodes: []graph.NodeDefinition{
			{ID: "run-tool", Type: "script", Config: rawConfig("runTool()")},
			{ID: "steer", Type: "script", Config: rawConfig("drainIntoBoard()")},
			{ID: "observe", Type: "script", Config: rawConfig("readBoard()")},
		},
		Edges: []graph.EdgeDefinition{
			{From: "run-tool", To: "steer"},
			{From: "steer", To: "observe"},
		},
	}, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "run the suite"))
	if err := executeGraphWithHost(g, host, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !drainedAtTool {
		t.Error("the steer node must drain right after the tool result, before the next round's content")
	}
	if drainedText != steerText {
		t.Errorf("drained text = %q, want %q", drainedText, steerText)
	}
	if !nextStepSeen {
		t.Error("the node after the steer boundary never ran")
	}
	want := []string{"user", "assistant", "tool", "user"}
	if len(nextStepRoles) != len(want) {
		t.Fatalf("next step saw roles %v, want %v", nextStepRoles, want)
	}
	for i := range want {
		if nextStepRoles[i] != want[i] {
			t.Fatalf("next step saw roles %v, want %v", nextStepRoles, want)
		}
	}
	if left := host.SteeredMessages(); len(left) != 0 {
		t.Errorf("steer queue left %d messages behind", len(left))
	}
}
