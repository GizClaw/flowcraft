package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/tool"
)

func testCatalog(t *testing.T) tool.Catalog {
	t.Helper()
	reg, err := tool.NewRegistry([]tool.Source{
		source{tools: []tool.Tool{
			funcTool("ok", "fine"),
			tool.TextTool(
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
	if res.IsError || res.Content.Text() != "fine" {
		t.Fatalf("result = %+v", res)
	}

	res = exec.Execute(context.Background(), message.ToolCall{ID: "c2", Name: "missing", Arguments: []byte(`{}`)})
	if !res.IsError || !strings.Contains(res.Content.Text(), "not found") {
		t.Fatalf("missing result = %+v", res)
	}

	res = exec.Execute(context.Background(), message.ToolCall{ID: "c3", Name: "boom", Arguments: []byte(`{}`)})
	if !res.IsError || !strings.Contains(res.Content.Text(), "kaboom") {
		t.Fatalf("error result = %+v", res)
	}
}

func TestExecutor_PreservesMultipartContent(t *testing.T) {
	source, err := media.NewImageBytes([]byte{1, 2, 3}, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	shot := tool.FuncTool(
		message.ToolDefinition{Name: "shot", InputSchema: []byte(`{"type":"object"}`)},
		func(context.Context, string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: "captured"},
				message.ImagePart{Source: source},
			}}, nil
		},
	)
	exec := tool.NewExecutor(catalogWithTools(t, shot))
	res := exec.Execute(context.Background(), message.ToolCall{ID: "c1", Name: "shot", Arguments: []byte(`{}`)})
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[1].(message.ImagePart); !ok {
		t.Fatalf("part 1 = %T, want message.ImagePart", res.Content.Parts[1])
	}
}

// catalogWithTools builds a catalog from arbitrary tools, mirroring
// testCatalog's registry wiring.
func catalogWithTools(t *testing.T, tools ...tool.Tool) tool.Catalog {
	t.Helper()
	reg, err := tool.NewRegistry([]tool.Source{source{tools: tools}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func TestExecutor_NormalizesPartlessContent(t *testing.T) {
	silent := tool.FuncTool(
		message.ToolDefinition{Name: "silent", InputSchema: []byte(`{"type":"object"}`)},
		func(context.Context, string) (message.Content, error) { return message.Content{}, nil },
	)
	exec := tool.NewExecutor(catalogWithTools(t, silent))
	res := exec.Execute(context.Background(),
		message.ToolCall{ID: "c1", Name: "silent", Arguments: []byte(`{}`)})
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want 1 (partless results are normalized)", len(res.Content.Parts))
	}
	if got := res.Content.Text(); got != "" {
		t.Fatalf("content = %q, want empty", got)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatalf("Marshal: %v", err)
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

func TestExecutor_InvalidContentBecomesErrorResult(t *testing.T) {
	for name, parts := range map[string][]message.Part{
		"nil part":     {nil},
		"empty source": {message.ImagePart{}},
		"empty data":   {message.DataPart{Value: []byte(`[1,2]`)}},
	} {
		t.Run(name, func(t *testing.T) {
			broken := tool.FuncTool(
				message.ToolDefinition{Name: "broken", InputSchema: []byte(`{"type":"object"}`)},
				func(context.Context, string) (message.Content, error) {
					return message.Content{Parts: parts}, nil
				})
			exec := tool.NewExecutor(catalogWithTools(t, broken))
			res := exec.Execute(context.Background(),
				message.ToolCall{ID: "c1", Name: "broken", Arguments: []byte(`{}`)})
			if !res.IsError {
				t.Fatalf("result = %+v, want an error result", res)
			}
			if got := res.Content.Text(); !strings.Contains(got, "invalid content") {
				t.Fatalf("content = %q, want it to name the invalid content", got)
			}
			if err := res.Validate(); err != nil {
				t.Fatalf("error result must itself be valid: %v", err)
			}
		})
	}
}
