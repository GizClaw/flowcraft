package tool_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func lazyWeather(name string, loads *atomic.Int32) tool.LazyTool {
	return tool.LazyTool{
		Name: name,
		Placeholder: message.ToolDefinition{
			Name:        name,
			Description: "placeholder description",
			InputSchema: []byte(`{"type":"object"}`),
		},
		Load: func(context.Context) (tool.Tool, error) {
			loads.Add(1)
			return tool.FuncTool(
				message.ToolDefinition{
					Name:        name,
					Description: "weather lookup for cities",
					InputSchema: []byte(`{"type":"object"}`),
				},
				func(context.Context, string) (string, error) { return "sunny", nil },
			), nil
		},
	}
}

func TestSearchWithLoad_PolicyToggle(t *testing.T) {
	var loads atomic.Int32
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{lazyTools: []tool.LazyTool{lazyWeather("weather", &loads)}}},
		tool.WithDynamic(tool.Policy{Default: tool.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()

	hits, err := session.Search(context.Background(), "cities", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("lazy search matched placeholder metadata: %+v", hits)
	}
	if loads.Load() != 0 {
		t.Fatalf("lazy search loaded %d tools, want 0", loads.Load())
	}

	// Same policy with SearchWithLoad on: tool_search's Search step
	// loads first and ranks real metadata.
	assembly2, err := tool.NewAssembly(
		[]tool.Source{source{lazyTools: []tool.LazyTool{lazyWeather("weather", &loads)}}},
		tool.WithDynamic(tool.Policy{Default: tool.ExposureDeferred, SearchWithLoad: true}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session2 := assembly2.NewSession()
	hits, err = session2.Search(context.Background(), "cities", 8)
	if err != nil {
		t.Fatalf("SearchWithLoad policy: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "weather" {
		t.Fatalf("hits = %+v, want [weather]", hits)
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}
}

func TestSearchWithLoad_ExplicitMethod(t *testing.T) {
	var loads atomic.Int32
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{lazyTools: []tool.LazyTool{lazyWeather("weather", &loads)}}},
		tool.WithDynamic(tool.Policy{Default: tool.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	hits, err := session.SearchWithLoad(context.Background(), "cities", 8)
	if err != nil {
		t.Fatalf("SearchWithLoad: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "weather" {
		t.Fatalf("hits = %+v, want [weather]", hits)
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}
}

func TestSession_LoadLoadsAllLazyTools(t *testing.T) {
	var loads atomic.Int32
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{lazyTools: []tool.LazyTool{
			lazyWeather("weather", &loads),
			lazyWeather("weather2", &loads),
		}}},
		tool.WithDynamic(tool.Policy{Default: tool.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	if err := session.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loads.Load() != 2 {
		t.Fatalf("loads = %d, want 2", loads.Load())
	}

	// Loaded proxies now serve their real definitions.
	session.Require("weather", "weather2")
	defs := session.Definitions()
	descriptions := map[string]string{}
	for _, def := range defs {
		descriptions[def.Name] = def.Description
	}
	for _, name := range []string{"weather", "weather2"} {
		if descriptions[name] != "weather lookup for cities" {
			t.Fatalf("definition %s still serves placeholder: %q", name, descriptions[name])
		}
	}
}

func TestStaticSession_LoadAndSearchWithLoad(t *testing.T) {
	assembly, err := tool.NewAssembly([]tool.Source{
		source{tools: []tool.Tool{funcTool("a", "1")}},
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	if err := session.Load(context.Background()); err != nil {
		t.Fatalf("static Load = %v, want nil", err)
	}
	if _, err := session.SearchWithLoad(context.Background(), "a", 8); !errdefs.IsNotAvailable(err) {
		t.Fatalf("static SearchWithLoad = %v, want NotAvailable", err)
	}
}
