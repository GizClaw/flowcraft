package tool

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// mapCatalog is the smallest Catalog, standing in for a registry.
type mapCatalog map[string]Tool

func (c mapCatalog) Get(name string) (Tool, bool) {
	t, ok := c[name]
	return t, ok
}

func (c mapCatalog) Definitions() []message.ToolDefinition {
	out := make([]message.ToolDefinition, 0, len(c))
	for _, t := range c {
		out = append(out, t.Definition())
	}
	return out
}

func echoCatalog(fn func(args string) string) mapCatalog {
	return mapCatalog{"echo": TextTool(
		message.ToolDefinition{Name: "echo", Description: "echo"},
		func(_ context.Context, args string) (string, error) { return fn(args), nil },
	)}
}

func TestExecutorDispatches(t *testing.T) {
	res := NewExecutor(echoCatalog(func(args string) string { return args })).
		Execute(context.Background(), message.ToolCall{ID: "c1", Name: "echo", Arguments: []byte("hi")})
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if res.Content.Text() != "hi" {
		t.Fatalf("content = %q", res.Content.Text())
	}
	if res.CallID != "c1" {
		t.Fatalf("call id = %q", res.CallID)
	}
}

func TestExecutorUnknownToolIsErrorResult(t *testing.T) {
	res := NewExecutor(mapCatalog{}).Execute(context.Background(),
		message.ToolCall{ID: "c1", Name: "nope"})
	if !res.IsError {
		t.Fatal("unknown tool must be an error result, not a Go error")
	}
}

func TestExecutorRecoversPanic(t *testing.T) {
	catalog := mapCatalog{"boom": FuncTool(
		message.ToolDefinition{Name: "boom"},
		func(context.Context, string) (message.Content, error) { panic("kaboom") },
	)}
	res := NewExecutor(catalog).Execute(context.Background(),
		message.ToolCall{ID: "c1", Name: "boom"})
	if !res.IsError {
		t.Fatal("a panicking tool must surface as an error result")
	}
}

func TestMiddlewareOrder(t *testing.T) {
	var order []string
	catalog := mapCatalog{"echo": TextTool(
		message.ToolDefinition{Name: "echo"},
		func(context.Context, string) (string, error) {
			order = append(order, "tool")
			return "ok", nil
		},
	)}
	mark := func(label string) Middleware {
		return func(next Dispatch) Dispatch {
			return func(ctx context.Context, call message.ToolCall) message.ToolResult {
				order = append(order, label)
				return next(ctx, call)
			}
		}
	}
	NewExecutor(catalog, mark("outer"), mark("inner")).Execute(context.Background(),
		message.ToolCall{ID: "c1", Name: "echo"})

	want := []string{"outer", "inner", "tool"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
