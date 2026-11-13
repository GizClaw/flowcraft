package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func benchDescriptor() model.ModelDescriptor {
	return model.ModelDescriptor{
		ID: model.ModelID{Provider: ProviderID, Name: "doubao-pro"},
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartText},
		},
	}
}

func benchImagePart(tb testing.TB, size int) message.Part {
	tb.Helper()
	data := make([]byte, size)
	source, err := media.NewImageBytes(data, "image/png")
	if err != nil {
		tb.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

func benchTextTools(tb testing.TB) inference.Toolset {
	tb.Helper()
	tools, err := inference.NewToolset([]message.ToolDefinition{{
		Name:        "search_records",
		Description: "Search records by free-text query and return ranked matches.",
		InputSchema: []byte(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`),
	}})
	if err != nil {
		tb.Fatalf("NewToolset: %v", err)
	}
	return tools
}

// BenchmarkCompileGenerate is the request-compilation half of a model call:
// canonical request in, provider wire request out, no network. It covers the
// conversation lowering, the tool mapping and — with text only — the cheap path.
func BenchmarkCompileGenerate(b *testing.B) {
	compile := compileGenerate("ep-20260101", benchDescriptor())
	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "doubao-pro"}},
		Output: inference.OutputText,
		Context: []message.Message{
			message.NewTextMessage(message.RoleSystem, "You are a careful assistant."),
			message.NewTextMessage(message.RoleUser, "Summarise the deployment in one paragraph."),
			message.NewTextMessage(message.RoleAssistant, "It is a Go agent runtime."),
		},
		Input: inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("Now list the risks.")},
		Tools: benchTextTools(b),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := compile(context.Background(), request, protocol.ShapeUnary); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompileGenerateWithImage is the same compile with an inline image in
// the history: the driver re-renders that payload on every turn, which is the
// cost a media-heavy conversation pays per request.
func BenchmarkCompileGenerateWithImage(b *testing.B) {
	compile := compileGenerate("ep-20260101", benchDescriptor())
	history := []message.Message{
		message.NewTextMessage(message.RoleUser, "What is in this screenshot?"),
		{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{benchImagePart(b, 512<<10)}}},
	}
	request := inference.GenerateRequest{
		Model:   model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "doubao-pro"}},
		Output:  inference.OutputText,
		Context: history,
		Input:   inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("Describe it again.")},
		Tools:   benchTextTools(b),
	}
	b.ReportAllocs()
	b.SetBytes(512 << 10)
	for i := 0; i < b.N; i++ {
		if _, err := compile(context.Background(), request, protocol.ShapeUnary); err != nil {
			b.Fatal(err)
		}
	}
}
