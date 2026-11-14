package nodes_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func history10k() graph.Messages {
	call, _ := message.NewToolCall("c1", "read_file", map[string]any{"file_path": "/tmp/x"})
	out := graph.EmptyMessages()
	for i := 0; i < 10_000; i++ {
		if i%3 == 0 {
			out = out.Append(message.Message{
				Role:    message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}},
			})
			continue
		}
		out = out.Append(message.NewTextMessage(message.RoleUser, "turn"))
	}
	return out
}

func BenchmarkPartSlice(b *testing.B) {
	m := history10k()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(m.Slice()) != 10_000 {
			b.Fatal("wrong length")
		}
	}
}

func BenchmarkPartDefinitions(b *testing.B) {
	provider := &stubProvider{response: textResponse("ok")}
	rt := dynamicRuntime(b, provider)
	view := rt.Assembly().NewCatalogView()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = view.Definitions()
	}
}

func BenchmarkPartToolset(b *testing.B) {
	provider := &stubProvider{response: textResponse("ok")}
	rt := dynamicRuntime(b, provider)
	defs := rt.Tools().Definitions()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := inference.NewToolset(defs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPartNewView(b *testing.B) {
	provider := &stubProvider{response: textResponse("ok")}
	rt := dynamicRuntime(b, provider)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = rt.Assembly().NewCatalogView()
	}
}
