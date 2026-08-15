package tool_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestAssembly_DynamicRegistersSearchTool(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("search", "no")}}},
		tool.WithDynamic(tool.Policy{Default: tool.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if _, ok := assembly.Catalog().Get(tool.ToolName); !ok {
		t.Fatal("tool_search not registered on dynamic assembly")
	}
	session := assembly.NewSession()
	names := definitionNames(session.Definitions())
	if !contains(names, tool.ToolName) {
		t.Fatalf("tool_search not visible: %v", names)
	}
}

func TestAssembly_WithoutDynamicNoSearchTool(t *testing.T) {
	assembly, err := tool.NewAssembly([]tool.Source{
		source{tools: []tool.Tool{funcTool("a", "1")}},
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if _, ok := assembly.Catalog().Get(tool.ToolName); ok {
		t.Fatal("tool_search must not be registered without dynamic")
	}
}

func TestAssembly_SearchToolExecutesAgainstSession(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("web.search", "find things")}}},
		tool.WithDynamic(tool.Policy{Default: tool.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	ctx := tool.WithSession(context.Background(), session)
	search, _ := assembly.Catalog().Get(tool.ToolName)
	out, err := search.Execute(ctx, `{"query":"web search","select":["web.search"]}`)
	if err != nil {
		t.Fatalf("Execute tool_search: %v", err)
	}
	if !strings.Contains(out, `"web.search"`) {
		t.Fatalf("tool_search output = %s", out)
	}
	names := definitionNames(session.Definitions())
	if !contains(names, "web.search") {
		t.Fatalf("selected tool not visible next round: %v", names)
	}
}

func TestAssembly_DispatcherDelegation(t *testing.T) {
	assembly, err := tool.NewAssembly([]tool.Source{
		source{tools: []tool.Tool{funcTool("ok", "fine")}},
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	var dispatcher tool.Dispatcher = assembly
	res := dispatcher.Execute(context.Background(),
		message.ToolCall{ID: "c", Name: "ok", Arguments: []byte(`{}`)})
	if res.IsError || res.Content != "fine" {
		t.Fatalf("result = %+v", res)
	}
}

func TestAssembly_CloseReleasesRegistry(t *testing.T) {
	assembly, err := tool.NewAssembly([]tool.Source{
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
	assembly, err := tool.NewAssembly([]tool.Source{attacher})
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
