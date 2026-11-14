package dashscope

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func benchSpec() ModelSpec {
	return ModelSpec{
		Name: "qwen-plus",
		Kind: "generate",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartText},
		},
	}
}

// benchMultimodalSpec declares the multimodal-generation surface, which is the
// only one that accepts typed media entries.
func benchMultimodalSpec() ModelSpec {
	spec := benchSpec()
	spec.Name = "qwen-vl-plus"
	spec.Generate.API = generateAPIMultimodal
	return spec
}

func benchImagePart(tb testing.TB, size int) message.Part {
	tb.Helper()
	source, err := media.NewImageBytes(make([]byte, size), "image/png")
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
// canonical request in, provider wire request out, no network.
func BenchmarkCompileGenerate(b *testing.B) {
	compile := compileGenerate(benchSpec(), "qwen-plus")
	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "qwen-plus"}},
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

// BenchmarkCompileGenerateWithImage covers the inline-media path: the same
// payload is re-rendered as a data URL on every request that carries it.
func BenchmarkCompileGenerateWithImage(b *testing.B) {
	compile := compileGenerate(benchMultimodalSpec(), "qwen-vl-plus")
	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "qwen-vl-plus"}},
		Output: inference.OutputText,
		Context: []message.Message{
			// An earlier user turn carried the image; the assistant answered in
			// text. Replaying that turn is what re-renders the payload.
			{Role: message.RoleUser, Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "What is in this screenshot?"},
				benchImagePart(b, 512<<10),
			}}},
			message.NewTextMessage(message.RoleAssistant, "A dashboard with three panels."),
		},
		Input: inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("Describe it again.")},
		Tools: benchTextTools(b),
	}
	b.ReportAllocs()
	b.SetBytes(512 << 10)
	for i := 0; i < b.N; i++ {
		if _, err := compile(context.Background(), request, protocol.ShapeUnary); err != nil {
			b.Fatal(err)
		}
	}
}
