package starlark

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/message"
)

// BenchmarkToStarlarkTranscript isolates the conversion half of a chat-shaped
// rule: the Go-side project result turned into Starlark values.
func BenchmarkToStarlarkTranscript(b *testing.B) {
	history := make([]message.Message, 0, 200)
	for i := 0; i < 200; i++ {
		role := message.RoleUser
		if i%2 == 1 {
			role = message.RoleAssistant
		}
		history = append(history, message.NewTextMessage(role, "这是对话里的一条消息，用来撑出真实的投影成本。"))
	}
	value := graph.MessagesValue(graph.MessagesFromSlice(history))
	collapsed, err := value.Messages()
	if err != nil {
		b.Fatalf("messages: %v", err)
	}
	// The projection the node now hands a backend: the sequence itself.
	projected := script.Messages{History: collapsed}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := toStarlark(projected, nil); err != nil {
			b.Fatalf("toStarlark: %v", err)
		}
	}
}
