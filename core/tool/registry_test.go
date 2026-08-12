package tool_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func funcTool(name, content string) tool.Tool {
	return tool.FuncTool(
		message.ToolDefinition{Name: name, InputSchema: []byte(`{"type":"object"}`)},
		func(context.Context, string) (string, error) { return content, nil },
	)
}

type source struct {
	tools     []tool.Tool
	lazyTools []tool.LazyTool
}

func (s source) Tools() []tool.Tool         { return s.tools }
func (s source) LazyTools() []tool.LazyTool { return s.lazyTools }

func TestRegistry_AggregatesSources(t *testing.T) {
	reg, err := tool.NewRegistry([]tool.Source{
		source{tools: []tool.Tool{funcTool("zeta", "z"), funcTool("alpha", "a")}},
		source{tools: []tool.Tool{funcTool("mid", "m")}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	defs := reg.Definitions()
	if len(defs) != 3 || defs[0].Name != "alpha" || defs[1].Name != "mid" || defs[2].Name != "zeta" {
		t.Fatalf("Definitions = %v, want sorted alpha/mid/zeta", defs)
	}
	if got, ok := reg.Get("zeta"); !ok || !strings.Contains(mustExecute(t, got), "z") {
		t.Fatalf("Get(zeta) = %v, %v", got, ok)
	}
}

func TestRegistry_DuplicateFailsFast(t *testing.T) {
	_, err := tool.NewRegistry([]tool.Source{
		source{tools: []tool.Tool{funcTool("dup", "first")}},
		source{tools: []tool.Tool{funcTool("dup", "second")}},
	})
	if !errdefs.IsConflict(err) {
		t.Fatalf("duplicate error = %v, want Conflict", err)
	}
}

func TestRegistry_OverwriteRespectsSourceOrder(t *testing.T) {
	reg, err := tool.NewRegistry([]tool.Source{
		source{tools: []tool.Tool{funcTool("dup", "first")}},
		source{tools: []tool.Tool{funcTool("dup", "second")}},
	}, tool.WithConflictPolicy(tool.ConflictOverwrite))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}
	got, _ := reg.Get("dup")
	if !strings.Contains(mustExecute(t, got), "second") {
		t.Fatalf("overwritten tool executes %q, want second", mustExecute(t, got))
	}
}

func TestRegistry_LazyToolLoadsOnceOnExecute(t *testing.T) {
	var loads atomic.Int32
	reg, err := tool.NewRegistry([]tool.Source{
		source{lazyTools: []tool.LazyTool{{
			Name:        "lazy",
			Placeholder: message.ToolDefinition{Name: "lazy", InputSchema: []byte(`{"type":"object"}`)},
			Load: func(context.Context) (tool.Tool, error) {
				loads.Add(1)
				return funcTool("lazy", "loaded"), nil
			},
		}}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	defs := reg.Definitions()
	if len(defs) != 1 || defs[0].Name != "lazy" || loads.Load() != 0 {
		t.Fatalf("Definitions = %v, loads = %d; lazy must not load at build time", defs, loads.Load())
	}

	got, _ := reg.Get("lazy")
	for i := 0; i < 3; i++ {
		if out := mustExecute(t, got); out != "loaded" {
			t.Fatalf("execute %d = %q, want loaded", i, out)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1 (load-once)", loads.Load())
	}
}

func TestRegistry_CloseForbidsLazyLoadsAndClosesInner(t *testing.T) {
	var closed atomic.Bool
	reg, err := tool.NewRegistry([]tool.Source{
		source{lazyTools: []tool.LazyTool{{
			Name:        "lazy",
			Placeholder: message.ToolDefinition{Name: "lazy", InputSchema: []byte(`{"type":"object"}`)},
			Load: func(context.Context) (tool.Tool, error) {
				return &closableTool{tool: funcTool("lazy", "x"), closed: &closed}, nil
			},
		}}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, _ := reg.Get("lazy")
	if out := mustExecute(t, got); out != "x" {
		t.Fatalf("execute = %q", out)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed.Load() {
		t.Fatal("inner lazy tool was not closed")
	}
	if _, err := got.Execute(context.Background(), "{}"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Execute after close = %v, want NotAvailable", err)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}
}

type closableTool struct {
	tool   tool.Tool
	closed *atomic.Bool
}

func (c *closableTool) Definition() message.ToolDefinition { return c.tool.Definition() }
func (c *closableTool) Execute(ctx context.Context, args string) (string, error) {
	return c.tool.Execute(ctx, args)
}
func (c *closableTool) Close() error {
	c.closed.Store(true)
	return nil
}

func mustExecute(t *testing.T, tl tool.Tool) string {
	t.Helper()
	out, err := tl.Execute(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func TestRegistry_RejectsNilSourceAndBadLazyTool(t *testing.T) {
	if _, err := tool.NewRegistry([]tool.Source{nil}); !errdefs.IsValidation(err) {
		t.Fatalf("nil source error = %v, want Validation", err)
	}
	_, err := tool.NewRegistry([]tool.Source{
		source{lazyTools: []tool.LazyTool{{Name: "x", Load: func(context.Context) (tool.Tool, error) {
			return nil, errors.New("never")
		}}}},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("placeholder mismatch error = %v, want Validation", err)
	}
}
