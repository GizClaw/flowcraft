package nodes

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestToolCallsOfReadsBothPartForms is the assertion the engine's tool loop depends on:
// either T or *T satisfies message.Part, and a driver that builds the pointer is as
// valid as one that builds the value. A raw type assertion reads only the value form, so
// a stray pointer used to mean "the model asked for no tools" — a silent empty round
// rather than a failure.
func TestToolCallsOfReadsBothPartForms(t *testing.T) {
	call, err := message.NewToolCall("call-1", "read_file", map[string]any{"file_path": "/tmp/x"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	for name, part := range map[string]message.Part{
		"value":   message.ToolCallPart{Call: call},
		"pointer": &message.ToolCallPart{Call: call},
	} {
		t.Run(name, func(t *testing.T) {
			msg := message.Message{
				Role:    message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "looking"}, part}},
			}
			calls := toolCallsOf(msg)
			if len(calls) != 1 || calls[0].ID != "call-1" || calls[0].Name != "read_file" {
				t.Fatalf("toolCallsOf = %+v, want the one call", calls)
			}
		})
	}
}
