package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/inference/pipeline"
)

// BenchmarkCompile is the per-request half of the pipeline: a canonical request
// becomes the SDK's params. It runs on every call, so the tool-schema cache and the
// part conversion are the two places an allocation regression hides.
func BenchmarkCompile(b *testing.B) {
	definition := message.ToolDefinition{
		Name:        "search",
		Description: "search the corpus",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	}
	tools, err := inference.NewToolset([]message.ToolDefinition{definition})
	if err != nil {
		b.Fatalf("NewToolset: %v", err)
	}

	d := testDriver(Settings{APIKey: "bench"})
	request := inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "claude-3-5-sonnet"}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hello"),
		},
		Output: inference.OutputText,
		Tools:  tools,
	}
	if err := request.Validate(); err != nil {
		b.Fatalf("request: %v", err)
	}
	ctx := context.Background()

	// Warm up: the first compile also decodes and memoises the tool schema.
	if _, err := d.compile(ctx, request, pipeline.Shape("")); err != nil {
		b.Fatalf("compile: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := d.compile(ctx, request, pipeline.Shape("")); err != nil {
			b.Fatalf("compile: %v", err)
		}
	}
}
