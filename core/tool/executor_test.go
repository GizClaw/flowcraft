package tool_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func testCatalog(t *testing.T) tool.Catalog {
	t.Helper()
	reg, err := tool.NewRegistry([]tool.Source{
		source{tools: []tool.Tool{
			funcTool("ok", "fine"),
			tool.FuncTool(
				message.ToolDefinition{Name: "boom", InputSchema: []byte(`{"type":"object"}`)},
				func(context.Context, string) (string, error) {
					return "", errors.New("kaboom")
				},
			),
		}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func TestExecutor_Execute(t *testing.T) {
	exec := tool.NewExecutor(testCatalog(t))
	res := exec.Execute(context.Background(), message.ToolCall{ID: "c1", Name: "ok", Arguments: []byte(`{}`)})
	if res.IsError || res.Content != "fine" {
		t.Fatalf("result = %+v", res)
	}

	res = exec.Execute(context.Background(), message.ToolCall{ID: "c2", Name: "missing", Arguments: []byte(`{}`)})
	if !res.IsError || !strings.Contains(res.Content, "not found") {
		t.Fatalf("missing result = %+v", res)
	}

	res = exec.Execute(context.Background(), message.ToolCall{ID: "c3", Name: "boom", Arguments: []byte(`{}`)})
	if !res.IsError || !strings.Contains(res.Content, "kaboom") {
		t.Fatalf("error result = %+v", res)
	}
}

func TestExecutor_ExecuteAllPreservesOrder(t *testing.T) {
	exec := tool.NewExecutor(testCatalog(t))
	calls := []message.ToolCall{
		{ID: "a", Name: "ok", Arguments: []byte(`{}`)},
		{ID: "b", Name: "missing", Arguments: []byte(`{}`)},
		{ID: "c", Name: "ok", Arguments: []byte(`{}`)},
	}
	results := exec.ExecuteAll(context.Background(), calls)
	if len(results) != 3 {
		t.Fatalf("results len = %d", len(results))
	}
	if results[0].CallID != "a" || results[0].IsError ||
		!results[1].IsError ||
		results[2].CallID != "c" || results[2].IsError {
		t.Fatalf("results = %+v", results)
	}
}

func TestExecutor_MiddlewareChainOrder(t *testing.T) {
	var order []string
	mark := func(name string) tool.Middleware {
		return func(next tool.Dispatch) tool.Dispatch {
			return func(ctx context.Context, call message.ToolCall) message.ToolResult {
				order = append(order, name+":before")
				res := next(ctx, call)
				order = append(order, name+":after")
				return res
			}
		}
	}
	exec := tool.NewExecutor(testCatalog(t), mark("outer"), mark("inner"))
	exec.Execute(context.Background(), message.ToolCall{ID: "c", Name: "ok", Arguments: []byte(`{}`)})
	want := []string{"outer:before", "inner:before", "inner:after", "outer:after"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
