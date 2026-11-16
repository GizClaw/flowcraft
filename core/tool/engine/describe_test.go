package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// A deployment rewrites a description to make the model pick the tool by the words
// it actually thinks in. Discovery ranks on the same text, so the rewrite has to
// reach the search index too — otherwise the tool is findable by vocabulary nobody
// uses.
func TestAssembly_DiscoveryRanksOnTheDecoratedText(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			tooltest.FuncTool("find_things", "an unhelpful legacy description",
				func(context.Context, string) (string, error) { return "found", nil }),
		}}},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}),
		engine.WithDefinitionDecorator(func(def message.ToolDefinition) message.ToolDefinition {
			def.Description = "Search the warehouse for widgets and bolts"
			return def
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()
	ctx := engine.WithCatalogView(context.Background(), session)
	search, ok := assembly.Catalog().Get(engine.ToolName)
	if !ok {
		t.Fatal("tool_search not registered")
	}
	out, err := search.Execute(ctx, `{"query":"widgets"}`)
	if err != nil {
		t.Fatalf("Execute tool_search: %v", err)
	}
	if rendered := jsonOf(t, out); !strings.Contains(rendered, `"exposed":["find_things"]`) {
		t.Fatalf("tool_search output = %s, want find_things exposed by its new text", rendered)
	}

	// What the model reads next round is the decorated text as well.
	for _, def := range session.Definitions() {
		if def.Name != "find_things" {
			continue
		}
		if !strings.Contains(def.Description, "widgets") {
			t.Errorf("visible description = %q, want the decorated text", def.Description)
		}
		return
	}
	t.Fatal("find_things is not visible after discovery")
}

// The decorator runs where definitions are built, so a tool published after the
// assembly — how an MCP server attaches — is decorated like the ones wired at
// construction.
func TestAssembly_DecoratorCoversLatePublications(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("early", "1")}}},
		engine.WithDefinitionDecorator(func(def message.ToolDefinition) message.ToolDefinition {
			def.Description = "decorated " + def.Name
			return def
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if err := assembly.Catalog().(tool.Registrar).Add(funcTool("late", "2")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	seen := map[string]string{}
	for _, def := range assembly.Registry().Definitions() {
		seen[def.Name] = def.Description
	}
	if seen["early"] != "decorated early" || seen["late"] != "decorated late" {
		t.Errorf("descriptions = %v, want both decorated", seen)
	}
}
