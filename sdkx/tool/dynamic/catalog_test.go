package dynamic

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
)

func funcTool(name, desc string) sdktool.Tool {
	return sdktool.FuncTool(message.Definition{
		Name:        name,
		Description: desc,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, _ string) (string, error) {
		return "ran:" + name, nil
	})
}

func testCatalog(t *testing.T, reg *sdktool.Registry) *Catalog {
	t.Helper()
	c := New(reg,
		WithExposure("always_tool", ExposureAlways),
		WithExposure("direct_tool", ExposureDirect),
		WithExposure("deferred_tool", ExposureDeferred),
		WithExposure("hidden_tool", ExposureHidden),
		WithSelectedRetention(2),
		WithRecentWindow(1),
	)
	return c
}

func names(defs []message.Definition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func TestCatalog_BaselineDefinitions(t *testing.T) {
	reg := sdktool.NewRegistry()
	reg.Register(funcTool("always_tool", "alpha"))
	reg.Register(funcTool("direct_tool", "beta"))
	reg.Register(funcTool("deferred_tool", "gamma"))
	reg.Register(funcTool("hidden_tool", "delta"))
	c := testCatalog(t, reg)
	t.Cleanup(func() { _ = c.Close() })

	got := names(c.Definitions())
	want := []string{"always_tool"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Definitions = %v, want %v", got, want)
	}
}

func TestCatalog_RequiredAndSelected(t *testing.T) {
	reg := sdktool.NewRegistry()
	for _, name := range []string{"always_tool", "direct_tool", "deferred_tool", "hidden_tool"} {
		reg.Register(funcTool(name, "x"))
	}
	c := testCatalog(t, reg)
	t.Cleanup(func() { _ = c.Close() })

	c.Require("deferred_tool", "hidden_tool")
	c.Select("deferred_tool")
	got := names(c.Definitions())
	want := []string{"always_tool", "deferred_tool", "hidden_tool"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Definitions = %v, want %v", got, want)
	}
}

func TestCatalog_SelectedExpiresAfterRounds(t *testing.T) {
	reg := sdktool.NewRegistry()
	reg.Register(funcTool("always_tool", "x"))
	reg.Register(funcTool("deferred_tool", "x"))
	c := testCatalog(t, reg)
	t.Cleanup(func() { _ = c.Close() })

	c.Select("deferred_tool")
	if got := names(c.Definitions()); !reflect.DeepEqual(got, []string{"always_tool", "deferred_tool"}) {
		t.Fatalf("after select Definitions = %v", got)
	}
	c.AdvanceTurn()
	if got := names(c.Definitions()); !reflect.DeepEqual(got, []string{"always_tool", "deferred_tool"}) {
		t.Fatalf("after one turn Definitions = %v", got)
	}
	c.AdvanceTurn()
	if got := names(c.Definitions()); !reflect.DeepEqual(got, []string{"always_tool"}) {
		t.Fatalf("after two turns Definitions = %v, want selected expiry", got)
	}
}

func TestCatalog_RecordCallKeepsDirectVisible(t *testing.T) {
	reg := sdktool.NewRegistry()
	reg.Register(funcTool("always_tool", "x"))
	reg.Register(funcTool("direct_tool", "x"))
	c := testCatalog(t, reg)
	t.Cleanup(func() { _ = c.Close() })

	if got := names(c.Definitions()); !reflect.DeepEqual(got, []string{"always_tool"}) {
		t.Fatalf("baseline Definitions = %v", got)
	}
	c.RecordCall(message.Call{ID: "c1", Name: "direct_tool"})
	if got := names(c.Definitions()); !reflect.DeepEqual(got, []string{"always_tool", "direct_tool"}) {
		t.Fatalf("after RecordCall Definitions = %v", got)
	}
	c.AdvanceTurn()
	c.AdvanceTurn()
	if got := names(c.Definitions()); !reflect.DeepEqual(got, []string{"always_tool"}) {
		t.Fatalf("after window expiry Definitions = %v", got)
	}
}

func TestCatalog_SearchExcludesAlwaysAndHidden(t *testing.T) {
	reg := sdktool.NewRegistry()
	reg.Register(funcTool("always_tool", "alpha shared"))
	reg.Register(funcTool("direct_tool", "alpha shared"))
	reg.Register(funcTool("deferred_tool", "alpha shared"))
	reg.Register(funcTool("hidden_tool", "alpha shared"))
	c := testCatalog(t, reg)
	t.Cleanup(func() { _ = c.Close() })

	hits, err := c.Search(context.Background(), "alpha", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.Name)
	}
	want := []string{"deferred_tool", "direct_tool"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Search hits = %v, want %v", got, want)
	}
}

func TestCatalog_RegisterAndSetExposure(t *testing.T) {
	reg := sdktool.NewRegistry()
	c := New(reg, WithDefaultExposure(ExposureDeferred))
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Register(funcTool("late", "x"), ExposureAlways); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := names(c.Definitions()); !reflect.DeepEqual(got, []string{"late"}) {
		t.Fatalf("Definitions = %v, want [late]", got)
	}
	if err := c.SetExposure("late", ExposureHidden); err != nil {
		t.Fatalf("SetExposure: %v", err)
	}
	if got := names(c.Definitions()); len(got) != 0 {
		t.Fatalf("Definitions after hidden = %v, want empty", got)
	}
}

func TestCatalog_RegisterProxyLoadsOnDemand(t *testing.T) {
	reg := sdktool.NewRegistry()
	c := New(reg, WithDefaultExposure(ExposureDeferred))
	t.Cleanup(func() { _ = c.Close() })

	var loads atomic.Int32
	inner := funcTool("proxy_tool", "real description")
	if err := c.RegisterProxy("proxy_tool", func(_ context.Context) (sdktool.Tool, error) {
		loads.Add(1)
		return inner, nil
	}, ExposureAlways); err != nil {
		t.Fatalf("RegisterProxy: %v", err)
	}

	// Visible with a placeholder definition before loading.
	defs := c.Definitions()
	if len(defs) != 1 || defs[0].Name != "proxy_tool" {
		t.Fatalf("Definitions = %v, want proxy placeholder", names(defs))
	}
	if loads.Load() != 0 {
		t.Fatalf("loader ran during Definitions, want lazy: %d", loads.Load())
	}
	if defs[0].Description != "" {
		t.Errorf("placeholder description = %q, want empty", defs[0].Description)
	}

	tool, ok := c.Get("proxy_tool")
	if !ok {
		t.Fatal("proxy missing from catalog")
	}
	res, err := tool.Execute(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res != "ran:proxy_tool" {
		t.Fatalf("Execute = %q", res)
	}
	if loads.Load() != 1 {
		t.Errorf("loader calls = %d, want 1", loads.Load())
	}
	// After load, the real definition is served.
	if got := c.Definitions()[0].Description; got != "real description" {
		t.Errorf("definition description = %q, want real", got)
	}
}

func TestCatalog_EnsureLoadedUnknownTool(t *testing.T) {
	c := New(sdktool.NewRegistry())
	t.Cleanup(func() { _ = c.Close() })
	err := c.EnsureLoaded(context.Background(), "ghost")
	if !errdefs.IsNotFound(err) {
		t.Fatalf("EnsureLoaded = %v, want NotFound", err)
	}
}

func TestCatalog_SearchDoesNotWakeDeferredProxies(t *testing.T) {
	reg := sdktool.NewRegistry()
	c := New(reg, WithDefaultExposure(ExposureDeferred))
	t.Cleanup(func() { _ = c.Close() })

	var loads atomic.Int32
	if err := c.RegisterProxy("p", func(_ context.Context) (sdktool.Tool, error) {
		loads.Add(1)
		return funcTool("p", "alpha real"), nil
	}, ExposureDeferred, WithPlaceholder(message.Definition{
		Name:        "p",
		Description: "alpha declared",
	})); err != nil {
		t.Fatalf("RegisterProxy: %v", err)
	}

	hits, err := c.Search(context.Background(), "alpha", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if loads.Load() != 0 {
		t.Fatalf("Search woke the deferred proxy: %d loader calls", loads.Load())
	}
	if len(hits) != 1 || hits[0].Name != "p" {
		t.Fatalf("Search hits = %+v, want the declared proxy metadata", hits)
	}

	// The explicit opt-in wakes deferred proxies.
	hits, err = c.SearchWithLoad(context.Background(), "alpha", 10)
	if err != nil {
		t.Fatalf("SearchWithLoad: %v", err)
	}
	if loads.Load() != 1 {
		t.Fatalf("SearchWithLoad did not load the proxy: %d loader calls", loads.Load())
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Description, "real") {
		t.Fatalf("SearchWithLoad hits = %+v, want the loaded real metadata", hits)
	}

	// The proxy is now loaded; select must not load it again.
	if err := c.EnsureLoaded(context.Background(), "p"); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if loads.Load() != 1 {
		t.Errorf("loader calls after select = %d, want exactly 1", loads.Load())
	}
}

func TestCatalog_CloseIsIdempotentAndForbidsLoads(t *testing.T) {
	reg := sdktool.NewRegistry()
	c := New(reg)
	var loads atomic.Int32
	if err := c.RegisterProxy("p", func(_ context.Context) (sdktool.Tool, error) {
		loads.Add(1)
		return funcTool("p", "x"), nil
	}, ExposureDeferred); err != nil {
		t.Fatalf("RegisterProxy: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := c.EnsureLoaded(context.Background(), "p"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("EnsureLoaded after close = %v, want NotAvailable", err)
	}
	if loads.Load() != 0 {
		t.Errorf("loader ran after close: %d", loads.Load())
	}
}

func TestCatalog_DefinitionsSortedAndDeterministic(t *testing.T) {
	reg := sdktool.NewRegistry()
	reg.Register(funcTool("zeta", "x"))
	reg.Register(funcTool("alpha", "x"))
	reg.Register(funcTool("mid", "x"))
	c := New(reg, WithDefaultExposure(ExposureAlways))
	t.Cleanup(func() { _ = c.Close() })

	first := names(c.Definitions())
	second := names(c.Definitions())
	want := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Errorf("Definitions = %v / %v, want stable %v", first, second, want)
	}
}

func TestCatalog_NewRejectsBadPolicy(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for invalid policy")
		}
	}()
	New(sdktool.NewRegistry(), WithDefaultExposure(Exposure("bogus")))
}

func TestCatalog_SetExposureAfterClose(t *testing.T) {
	c := New(sdktool.NewRegistry())
	_ = c.Close()
	if err := c.SetExposure("x", ExposureAlways); !errdefs.IsNotAvailable(err) {
		t.Fatalf("SetExposure after close = %v, want NotAvailable", err)
	}
}
