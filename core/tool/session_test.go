package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func def(name, desc string) message.ToolDefinition {
	return message.ToolDefinition{
		Name:        name,
		Description: desc,
		InputSchema: []byte(`{"type":"object"}`),
	}
}

func dynamicAssembly(t *testing.T, names ...string) (*tool.Assembly, tool.Policy) {
	t.Helper()
	exposures := map[string]tool.Exposure{
		"always":   tool.ExposureAlways,
		"direct":   tool.ExposureDirect,
		"deferred": tool.ExposureDeferred,
		"hidden":   tool.ExposureHidden,
	}
	tools := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, funcTool(name, "out-"+name))
	}
	policy := tool.Policy{
		Default:   tool.ExposureDeferred,
		Exposures: exposures,
	}
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: tools}},
		tool.WithDynamic(policy),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	return assembly, policy
}

func TestDynamicSession_ExposureBaseline(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always", "direct", "deferred", "hidden")
	session := assembly.NewSession()

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

func TestDynamicSession_RequireSelectRecordAdvance(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always", "direct", "deferred", "hidden")
	session := assembly.NewSession()

	session.Require("hidden")
	names := definitionNames(session.Definitions())
	if !contains(names, "hidden") {
		t.Fatalf("required hidden tool missing: %v", names)
	}

	session.Select("deferred")
	names = definitionNames(session.Definitions())
	if !contains(names, "deferred") {
		t.Fatalf("selected deferred tool missing: %v", names)
	}

	session.RecordCall(message.ToolCall{ID: "c1", Name: "direct", Arguments: []byte(`{}`)})
	names = definitionNames(session.Definitions())
	if !contains(names, "direct") {
		t.Fatalf("recent direct tool missing: %v", names)
	}

	session.AdvanceTurn()
	names = definitionNames(session.Definitions())
	if !contains(names, "direct") || !contains(names, "deferred") {
		t.Fatalf("recent/selected tools must survive one advance: %v", names)
	}
	// recent window is 10, selected retention is 5 — advance past both.
	for i := 0; i < 11; i++ {
		session.AdvanceTurn()
	}
	names = definitionNames(session.Definitions())
	if contains(names, "direct") || contains(names, "deferred") {
		t.Fatalf("tools survived retention windows: %v", names)
	}
}

func TestDynamicSession_Search(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always", "direct", "deferred", "hidden")
	session := assembly.NewSession()

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
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			funcTool("a", "1"), funcTool("b", "2"), funcTool("c", "3"),
		}}},
		tool.WithDynamic(tool.Policy{
			Default: tool.ExposureAlways,
			Budget:  tool.Budget{MaxDefinitions: 2},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	names := definitionNames(session.Definitions())
	if len(names) != 2 {
		t.Fatalf("budget allowed %d definitions: %v", len(names), names)
	}
}

func TestStaticSession(t *testing.T) {
	assembly, err := tool.NewAssembly([]tool.Source{
		source{tools: []tool.Tool{funcTool("a", "1"), funcTool("b", "2")}},
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	if len(definitionNames(session.Definitions())) != 2 {
		t.Fatalf("static session must show every tool")
	}
	session.Select("a")
	session.AdvanceTurn()
	session.RecordCall(message.ToolCall{ID: "c", Name: "a", Arguments: []byte(`{}`)})
	if _, err := session.Search(context.Background(), "a", 8); !errdefs.IsNotAvailable(err) {
		t.Fatalf("static Search = %v, want NotAvailable", err)
	}
}

func TestSessionContext(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always")
	session := assembly.NewSession()
	ctx := tool.WithSession(context.Background(), session)
	got, ok := tool.SessionFromContext(ctx)
	if !ok || got != session {
		t.Fatalf("SessionFromContext = %v, %v", got, ok)
	}
	if _, ok := tool.SessionFromContext(context.Background()); ok {
		t.Fatal("SessionFromContext without session must be false")
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
