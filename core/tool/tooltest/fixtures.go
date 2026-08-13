package tooltest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// FuncTool builds a simple tool whose input schema is an empty JSON
// object.
func FuncTool(
	name, description string,
	fn func(context.Context, string) (string, error),
) tool.Tool {
	return tool.FuncTool(message.ToolDefinition{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, fn)
}

// Source adapts eager and lazy tools into a tool.Source.
func Source(tools ...tool.Tool) tool.Source {
	return source{tools: append([]tool.Tool(nil), tools...)}
}

type source struct {
	tools []tool.Tool
	lazy  []tool.LazyTool
}

func (s source) Tools() []tool.Tool         { return s.tools }
func (s source) LazyTools() []tool.LazyTool { return s.lazy }

// LazySource adapts lazy descriptors into a tool.Source.
func LazySource(lazy ...tool.LazyTool) tool.Source {
	return source{lazy: append([]tool.LazyTool(nil), lazy...)}
}

// LazyTool builds a valid lazy descriptor around a loader.
func LazyTool(
	name string,
	load func(context.Context) (tool.Tool, error),
) tool.LazyTool {
	return tool.LazyTool{
		Name:        name,
		Placeholder: message.ToolDefinition{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Load:        load,
	}
}

// Catalog builds a catalog from the supplied tools and registers cleanup.
func Catalog(t *testing.T, tools ...tool.Tool) tool.Catalog {
	t.Helper()
	registry := Registry(t, tools...)
	return registry
}

// Registry builds a tool.Registry from the supplied tools and registers
// cleanup.
func Registry(t *testing.T, tools ...tool.Tool) *tool.Registry {
	t.Helper()
	registry, err := tool.NewRegistry([]tool.Source{Source(tools...)})
	if err != nil {
		t.Fatalf("tooltest.Registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

// Execute runs one tool call and fails the test on transport or lookup
// errors.
func Execute(
	t *testing.T,
	catalog tool.Catalog,
	name, arguments string,
) (string, error) {
	t.Helper()
	executor := tool.NewExecutor(catalog)
	result := executor.Execute(context.Background(), message.ToolCall{
		ID:        "tooltest-call",
		Name:      name,
		Arguments: json.RawMessage(arguments),
	})
	if result.IsError {
		t.Fatalf("tooltest.Execute(%q): %s", name, result.Content)
	}
	return result.Content, nil
}
