package inference_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// benchToolset builds a toolset whose schemas are the size real providers ship:
// an MCP-style object with a handful of described properties.
func benchToolset(tb testing.TB, count, schemaBytes int) inference.Toolset {
	tb.Helper()
	properties := strings.Repeat("x", schemaBytes)
	schema := `{"type":"object","properties":{"query":{"type":"string","description":"` +
		properties + `"}}}`
	definitions := make([]message.ToolDefinition, 0, count)
	for i := 0; i < count; i++ {
		definitions = append(definitions, message.ToolDefinition{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: "Search records by free-text query and return ranked matches.",
			InputSchema: []byte(schema),
		})
	}
	tools, err := inference.NewToolset(definitions)
	if err != nil {
		tb.Fatalf("NewToolset: %v", err)
	}
	return tools
}

func benchHashRequest(tb testing.TB, tools inference.Toolset) inference.GenerateRequest {
	tb.Helper()
	return inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "p", Name: "m"}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hi"),
		},
		Context: []message.Message{message.NewTextMessage(message.RoleUser, "ctx")},
		Tools:   tools,
	}
}

// BenchmarkRequestHashTools isolates the tool-definition half of the request
// digest: RequestHashWith re-encodes every definition on every recorded call.
// The trace path is the only caller (agent-graph computes it when node tracing
// or replay is on), so the number is the ceiling of that path's cost.
func BenchmarkRequestHashTools(b *testing.B) {
	for _, count := range []int{0, 20} {
		for _, schemaBytes := range []int{256, 1024} {
			if count == 0 && schemaBytes == 1024 {
				continue
			}
			request := benchHashRequest(b, benchToolset(b, count, schemaBytes))
			b.Run(fmt.Sprintf("tools=%d/schema=%d", count, schemaBytes), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = inference.RequestHash(request)
				}
			})
		}
	}
}
