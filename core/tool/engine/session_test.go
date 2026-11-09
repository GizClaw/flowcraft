package engine_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

func dynamicAssembly(t *testing.T, names ...string) (*engine.Assembly, engine.Policy) {
	t.Helper()
	exposures := map[string]engine.Exposure{
		"always":   engine.ExposureAlways,
		"direct":   engine.ExposureDirect,
		"deferred": engine.ExposureDeferred,
		"hidden":   engine.ExposureHidden,
	}
	tools := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, funcTool(name, "out-"+name))
	}
	policy := engine.Policy{
		Default:   engine.ExposureDeferred,
		Exposures: exposures,
	}
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: tools}},
		engine.WithDynamic(policy),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	return assembly, policy
}

func TestDynamicSession_ExposureBaseline(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always", "direct", "deferred", "hidden")
	session := assembly.NewCatalogView()

	names := definitionNames(session.Definitions())
	want := []string{"always", "tool_search"}
	if len(names) != len(want) {
		t.Fatalf("Definitions = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("Definitions = %v, want %v", names, want)
		}
	}
}

func TestDynamicSession_DiscoverRecordAdvance(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always", "direct", "deferred", "hidden")
	session := assembly.NewCatalogView()

	session.Require("hidden")
	names := definitionNames(session.Definitions())
	if !contains(names, "hidden") {
		t.Fatalf("required hidden tool missing: %v", names)
	}

	outcome := session.Discover("deferred")
	if len(outcome.Results) != 1 || !outcome.Results[0].Exposed {
		t.Fatalf("Discover deferred outcome = %+v, want exposed", outcome.Results)
	}
	names = definitionNames(session.Definitions())
	if !contains(names, "deferred") {
		t.Fatalf("discovered deferred tool missing: %v", names)
	}

	session.RecordCall(message.ToolCall{ID: "c1", Name: "direct", Arguments: message.NewArguments([]byte(`{}`))})
	names = definitionNames(session.Definitions())
	if !contains(names, "direct") {
		t.Fatalf("used direct tool missing: %v", names)
	}

	session.AdvanceRound()
	names = definitionNames(session.Definitions())
	if !contains(names, "direct") || !contains(names, "deferred") {
		t.Fatalf("used/discovered tools must survive one advance: %v", names)
	}
	// The discovery idle horizon is 10 — advance past it.
	for i := 0; i < 11; i++ {
		session.AdvanceRound()
	}
	names = definitionNames(session.Definitions())
	if contains(names, "direct") || contains(names, "deferred") {
		t.Fatalf("idle tools survived the discovery horizon: %v", names)
	}
}

func TestDynamicSession_Search(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always", "direct", "deferred", "hidden")
	session := assembly.NewCatalogView()

	hits, err := session.Search(context.Background(), "deferred", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "deferred" {
		t.Fatalf("hits = %+v, want [deferred]", hits)
	}
	hits, err = session.Search(context.Background(), "always", 8)
	if err != nil {
		t.Fatalf("Search always: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("always must not be searchable, hits = %+v", hits)
	}
	hits, err = session.Search(context.Background(), "hidden", 8)
	if err != nil {
		t.Fatalf("Search hidden: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hidden must not be searchable, hits = %+v", hits)
	}
}

func TestDynamicSession_Budget(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			funcTool("a", "1"), funcTool("b", "2"), funcTool("c", "3"),
		}}},
		engine.WithDynamic(engine.Policy{
			Default: engine.ExposureAlways,
			Budget:  engine.Budget{MaxDefinitions: 2},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()
	names := definitionNames(session.Definitions())
	if len(names) != 2 {
		t.Fatalf("budget allowed %d definitions: %v", len(names), names)
	}
}

func TestStaticSession(t *testing.T) {
	assembly, err := engine.NewAssembly([]tool.Source{
		source{tools: []tool.Tool{funcTool("a", "1"), funcTool("b", "2")}},
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()
	if len(definitionNames(session.Definitions())) != 2 {
		t.Fatalf("static session must show every tool")
	}
	session.Discover("a")
	session.AdvanceRound()
	session.RecordCall(message.ToolCall{ID: "c", Name: "a", Arguments: message.NewArguments([]byte(`{}`))})
	if _, err := session.Search(context.Background(), "a", 8); !errdefs.IsNotAvailable(err) {
		t.Fatalf("static Search = %v, want NotAvailable", err)
	}
}

func TestSessionContext(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always")
	session := assembly.NewCatalogView()
	ctx := engine.WithCatalogView(context.Background(), session)
	got, ok := engine.CatalogViewFrom(ctx)
	if !ok || got != session {
		t.Fatalf("CatalogViewFrom = %v, %v", got, ok)
	}
	if _, ok := engine.CatalogViewFrom(context.Background()); ok {
		t.Fatal("CatalogViewFrom without session must be false")
	}
}

func definitionNames(defs []message.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
