package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
)

func TestSearchTool_RequiresCatalogOnContext(t *testing.T) {
	_, err := NewSearchTool().Execute(context.Background(), `{"query":"x"}`)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Execute without catalog = %v, want NotAvailable", err)
	}
}

func TestSearchTool_ReturnsHitsAndSelects(t *testing.T) {
	reg := sdktool.NewRegistry()
	reg.Register(funcTool("deferred_tool", "alpha shared"))
	reg.Register(funcTool("direct_tool", "alpha shared"))
	c := New(reg,
		WithDefaultExposure(ExposureDeferred),
		WithExposure("direct_tool", ExposureDirect),
		WithSelectedRetention(2),
	)
	t.Cleanup(func() { _ = c.Close() })

	ctx := WithCatalog(context.Background(), c)
	raw, err := NewSearchTool().Execute(ctx, `{"query":"alpha","select":["deferred_tool"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result struct {
		Query string `json:"query"`
		Hits  []struct {
			Name string `json:"name"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, raw)
	}
	if result.Query != "alpha" {
		t.Errorf("query = %q", result.Query)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(result.Hits))
	}
	if !strings.Contains(raw, "deferred_tool") {
		t.Errorf("result does not contain deferred_tool: %s", raw)
	}

	defs := c.Definitions()
	got := names(defs)
	want := []string{"deferred_tool"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Definitions after select = %v, want %v", got, want)
	}
}

func TestSearchTool_RejectsBadInput(t *testing.T) {
	c := New(sdktool.NewRegistry())
	t.Cleanup(func() { _ = c.Close() })
	ctx := WithCatalog(context.Background(), c)

	if _, err := NewSearchTool().Execute(ctx, `{"limit":2}`); !errdefs.IsValidation(err) {
		t.Errorf("missing query = %v, want Validation", err)
	}
	if _, err := NewSearchTool().Execute(ctx, `not json`); !errdefs.IsValidation(err) {
		t.Errorf("bad json = %v, want Validation", err)
	}
}

func TestSearchTool_SelectLoadsRealSchemaForNextRound(t *testing.T) {
	reg := sdktool.NewRegistry()
	c := New(reg, WithDefaultExposure(ExposureDeferred), WithSelectedRetention(3))
	t.Cleanup(func() { _ = c.Close() })

	if err := c.RegisterProxy("deferred_tool", func(_ context.Context) (sdktool.Tool, error) {
		return sdktool.FuncTool(
			message.DefineSchema("deferred_tool", "alpha shared real description",
				message.ToolProperty("path", "string", "file path"),
			).Required("path").Build(),
			func(_ context.Context, _ string) (string, error) { return "ok", nil },
		), nil
	}, ExposureDeferred, WithPlaceholder(message.Definition{
		Name:        "deferred_tool",
		Description: "alpha shared declared description",
	})); err != nil {
		t.Fatalf("RegisterProxy: %v", err)
	}

	ctx := WithCatalog(context.Background(), c)
	raw, err := NewSearchTool().Execute(ctx,
		`{"query":"alpha","select":["deferred_tool"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result struct {
		Hits []struct {
			Name string `json:"name"`
		} `json:"hits"`
		Selected []string `json:"selected"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, raw)
	}
	if len(result.Selected) != 1 || result.Selected[0] != "deferred_tool" {
		t.Fatalf("selected = %v, want [deferred_tool]", result.Selected)
	}
	if len(result.Hits) != 1 || result.Hits[0].Name != "deferred_tool" {
		t.Fatalf("hits = %+v, want the declared proxy", result.Hits)
	}

	// Round N+1: the real definition, not the LazyTool placeholder.
	defs := c.Definitions()
	if len(defs) != 1 {
		t.Fatalf("Definitions = %v, want the selected tool", names(defs))
	}
	if defs[0].Description != "alpha shared real description" {
		t.Errorf("next-round description = %q, want real description", defs[0].Description)
	}
	if !strings.Contains(string(defs[0].InputSchema), "path") {
		t.Errorf("next-round schema = %s, want the loaded tool's real schema", defs[0].InputSchema)
	}
}

func TestSearchTool_SelectSkipsUnloadableTool(t *testing.T) {
	reg := sdktool.NewRegistry()
	c := New(reg, WithDefaultExposure(ExposureDeferred), WithSelectedRetention(3))
	t.Cleanup(func() { _ = c.Close() })

	if err := c.RegisterProxy("down", func(context.Context) (sdktool.Tool, error) {
		return nil, errors.New("server unreachable")
	}, ExposureDeferred, WithRetryPolicy(RetryPolicy{Attempts: 1, BaseDelay: 0})); err != nil {
		t.Fatalf("RegisterProxy: %v", err)
	}

	ctx := WithCatalog(context.Background(), c)
	raw, err := NewSearchTool().Execute(ctx,
		`{"query":"down","select":["down"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result struct {
		Selected []string `json:"selected"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, raw)
	}
	if len(result.Selected) != 0 {
		t.Fatalf("selected = %v, want empty for unloadable tool", result.Selected)
	}
	if got := names(c.Definitions()); len(got) != 0 {
		t.Errorf("unloadable tool was selected: %v", got)
	}
}

func TestSearchTool_Definition(t *testing.T) {
	def := NewSearchTool().Definition()
	if def.Name != ToolName {
		t.Errorf("name = %q, want %q", def.Name, ToolName)
	}
	if err := def.Validate(); err != nil {
		t.Errorf("definition invalid: %v", err)
	}
}

func TestRegisterSearchTool(t *testing.T) {
	reg := sdktool.NewRegistry()
	c := New(reg)
	t.Cleanup(func() { _ = c.Close() })
	if err := RegisterSearchTool(reg, c); err != nil {
		t.Fatalf("RegisterSearchTool: %v", err)
	}
	if got := names(c.Definitions()); len(got) != 1 || got[0] != ToolName {
		t.Errorf("Definitions = %v, want [%s]", got, ToolName)
	}
	if _, ok := reg.Get(ToolName); !ok {
		t.Fatal("tool_search missing from registry")
	}
}
