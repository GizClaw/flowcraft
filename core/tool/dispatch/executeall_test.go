package dispatch_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/dispatch"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// TestExecuteAllPreservesInputOrder keeps the zip between calls and results valid: a
// caller that pairs them up must never get result i for call j.
func TestExecuteAllPreservesInputOrder(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}
	tools := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, tool.TextTool(
			message.ToolDefinition{Name: name, InputSchema: []byte(`{"type":"object"}`)},
			func(_ context.Context, _ string) (string, error) { return name, nil }))
	}
	registry, err := tool.NewRegistry([]tool.Source{staticSource(tools)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	calls := make([]message.ToolCall, 0, len(names))
	for _, name := range names {
		calls = append(calls, message.ToolCall{
			ID:        name,
			Name:      name,
			Arguments: message.NewArguments([]byte(`{}`)),
		})
	}

	results := dispatch.New(registry).ExecuteAll(context.Background(), tooltest.Binding(), calls)
	if len(results) != len(calls) {
		t.Fatalf("results = %d, want %d", len(results), len(calls))
	}
	for i, result := range results {
		if result.CallID != calls[i].ID {
			t.Fatalf("result %d answers call %q, want %q", i, result.CallID, calls[i].ID)
		}
	}
}

// TestNilMiddlewareIsSkipped keeps a deployment that disables a knob (Truncate with no
// settings returns nil) from having to filter its own chain.
func TestNilMiddlewareIsSkipped(t *testing.T) {
	registry, err := tool.NewRegistry([]tool.Source{staticSource{benchTool()}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	executor := dispatch.New(registry, nil, nil)
	if res := executor.Execute(context.Background(), tooltest.Binding(), benchCall()); res.IsError {
		t.Fatalf("Execute: %s", res.Content.Text())
	}
}

// TestScratchPartitionIsolatesTwoTools pins the reason dispatch rewrites the session: two
// tools of one session using the same key must not see each other's value.
func TestScratchPartitionIsolatesTwoTools(t *testing.T) {
	store := func(name string) tool.Tool {
		return tool.CapFunc(nil,
			message.ToolDefinition{Name: name, InputSchema: []byte(`{"type":"object"}`)},
			func(_ context.Context, b tool.Binding, _ string) (message.Content, error) {
				b.Session.Scratch().Store("k", name)
				value, _ := b.Session.Scratch().Load("k")
				return message.NewTextContent(value.(string)), nil
			})
	}
	registry, err := tool.NewRegistry([]tool.Source{staticSource{store("a"), store("b")}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	executor := dispatch.New(registry)
	binding := tool.Bind(tooltest.Session("scratch"), policy.Grant{})

	for _, name := range []string{"a", "b"} {
		res := executor.Execute(context.Background(), binding, message.ToolCall{
			ID: name, Name: name, Arguments: message.NewArguments([]byte(`{}`)),
		})
		if res.IsError || res.Content.Text() != name {
			t.Fatalf("%s read %q (err=%v)", name, res.Content.Text(), res.IsError)
		}
	}
}
