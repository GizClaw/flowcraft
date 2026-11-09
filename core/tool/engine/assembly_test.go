package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// jsonOf returns the JSON an object result carries as its structured
// data part.
func jsonOf(t *testing.T, content message.Content) string {
	t.Helper()
	if len(content.Parts) != 1 {
		t.Fatalf("content parts = %d, want 1", len(content.Parts))
	}
	data, ok := content.Parts[0].(message.DataPart)
	if !ok {
		t.Fatalf("content part = %T, want message.DataPart", content.Parts[0])
	}
	return string(data.Value)
}

func TestAssembly_DynamicRegistersSearchTool(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("search", "no")}}},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if _, ok := assembly.Catalog().Get(engine.ToolName); !ok {
		t.Fatal("tool_search not registered on dynamic assembly")
	}
	session := assembly.NewCatalogView()
	names := definitionNames(session.Definitions())
	if !contains(names, engine.ToolName) {
		t.Fatalf("tool_search not visible: %v", names)
	}
}

func TestAssembly_WithoutDynamicNoSearchTool(t *testing.T) {
	assembly, err := engine.NewAssembly([]tool.Source{
		source{tools: []tool.Tool{funcTool("a", "1")}},
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if _, ok := assembly.Catalog().Get(engine.ToolName); ok {
		t.Fatal("tool_search must not be registered without dynamic")
	}
}

func TestAssembly_SearchToolExecutesAgainstSession(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("web.search", "find things")}}},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()
	ctx := engine.WithCatalogView(context.Background(), session)
	search, _ := assembly.Catalog().Get(engine.ToolName)
	out, err := search.Execute(ctx, `{"query":"web search"}`)
	if err != nil {
		t.Fatalf("Execute tool_search: %v", err)
	}
	if rendered := jsonOf(t, out); !strings.Contains(rendered, `"exposed":["web.search"]`) {
		t.Fatalf("tool_search output = %s", rendered)
	}
	names := definitionNames(session.Definitions())
	if !contains(names, "web.search") {
		t.Fatalf("discovered tool not visible next round: %v", names)
	}
}

func TestAssembly_SessionRecorderRefreshesDiscovery(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("web.search", "find things")}}},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()
	ctx := engine.WithCatalogView(context.Background(), session)

	res := assembly.Execute(ctx, message.ToolCall{
		ID: "c1", Name: "web.search", Arguments: message.NewArguments([]byte(`{}`)),
	})
	if res.IsError {
		t.Fatalf("Execute = %+v", res)
	}
	names := definitionNames(session.Definitions())
	if !contains(names, "web.search") {
		t.Fatalf("executed tool not in discovery pool: %v", names)
	}
}

func TestAssembly_DispatcherDelegation(t *testing.T) {
	assembly, err := engine.NewAssembly([]tool.Source{
		source{tools: []tool.Tool{funcTool("ok", "fine")}},
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	var dispatcher tool.Dispatcher = assembly
	res := dispatcher.Execute(context.Background(),
		message.ToolCall{ID: "c", Name: "ok", Arguments: message.NewArguments([]byte(`{}`))})
	if res.IsError || res.Content.Text() != "fine" {
		t.Fatalf("result = %+v", res)
	}
}

func TestAssembly_CloseReleasesRegistry(t *testing.T) {
	assembly, err := engine.NewAssembly([]tool.Source{
		source{tools: []tool.Tool{funcTool("a", "1")}},
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if err := assembly.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := assembly.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// attacherSource contributes one eager tool and publishes one runtime
// tool through the Registrar the assembly hands it.
type attacherSource struct {
	attached bool
}

func (a *attacherSource) Tools() []tool.Tool {
	return []tool.Tool{funcTool("eager", "eager")}
}

func (a *attacherSource) LazyTools() []tool.LazyTool { return nil }

func (a *attacherSource) Attach(r tool.Registrar) {
	a.attached = true
	if err := r.Add(funcTool("runtime", "runtime")); err != nil {
		panic(err)
	}
}

func TestAssembly_AttachesRegistryToSources(t *testing.T) {
	attacher := &attacherSource{}
	assembly, err := engine.NewAssembly([]tool.Source{attacher})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if !attacher.attached {
		t.Fatal("source never received the registrar")
	}
	if _, ok := assembly.Catalog().Get("runtime"); !ok {
		t.Fatal("runtime-published tool missing from catalog")
	}
	if _, ok := assembly.Catalog().Get("eager"); !ok {
		t.Fatal("eager tool missing from catalog")
	}
}
