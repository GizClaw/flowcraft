package script

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/message"
)

// BenchmarkProjectTranscript isolates the projection half of a chat-shaped rule,
// so an optimisation can be attributed to the right side of the bridge.
func BenchmarkProjectTranscript(b *testing.B) {
	history := make([]message.Message, 0, 200)
	for i := 0; i < 200; i++ {
		role := message.RoleUser
		if i%2 == 1 {
			role = message.RoleAssistant
		}
		history = append(history, message.NewTextMessage(role, "这是对话里的一条消息，用来撑出真实的投影成本。"))
	}
	values := map[string]graph.Value{
		"history": graph.MessagesValue(graph.MessagesFromSlice(history)),
	}
	in := graph.Invoke{
		Node:     "tally",
		Config:   json.RawMessage(`{"backend":"fake","path":"rule.star","reads":["history"],"writes":[]}`),
		State:    func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(values) },
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := project(in, []string{"history"}); err != nil {
			b.Fatalf("project: %v", err)
		}
	}
}
