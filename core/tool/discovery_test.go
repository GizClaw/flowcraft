package tool_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

func TestDynamicSession_DiscoveryPoolEvictsLRU(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			funcTool("a", "1"), funcTool("b", "2"),
			funcTool("c", "3"), funcTool("d", "4"),
		}}},
		tool.WithDynamic(tool.Policy{
			Default:   tool.ExposureDeferred,
			Discovery: tool.DiscoveryPolicy{MaxTools: 2},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()

	outcome := session.Discover("a", "b", "c", "d")
	if len(outcome.Results) != 4 ||
		outcome.Results[0].Exposed || outcome.Results[1].Exposed ||
		!outcome.Results[2].Exposed || !outcome.Results[3].Exposed {
		t.Fatalf("Discover outcome = %+v, want a/b evicted, c/d exposed", outcome)
	}
	if len(outcome.Evicted) != 2 || outcome.Evicted[0] != "a" || outcome.Evicted[1] != "b" {
		t.Fatalf("evicted = %v, want [a b]", outcome.Evicted)
	}

	names := definitionNames(session.Definitions())
	if contains(names, "a") || contains(names, "b") {
		t.Fatalf("evicted tools still visible: %v", names)
	}
	if !contains(names, "c") || !contains(names, "d") || !contains(names, tool.ToolName) {
		t.Fatalf("Definitions = %v, want c, d and tool_search", names)
	}

	// Re-discovering an evicted tool refreshes it as MRU and evicts the
	// least-recently-used survivor instead.
	outcome = session.Discover("a")
	if len(outcome.Results) != 1 || !outcome.Results[0].Exposed {
		t.Fatalf("refresh Discover outcome = %+v", outcome)
	}
	if len(outcome.Evicted) != 1 || outcome.Evicted[0] != "c" {
		t.Fatalf("refresh evicted = %v, want [c]", outcome.Evicted)
	}
}

func TestDynamicSession_DiscoveryByteCap(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			funcTool("a", "a"),
			tooltest.FuncTool("big", strings.Repeat("x", 4096), func(context.Context, string) (string, error) {
				return "big", nil
			}),
		}}},
		tool.WithDynamic(tool.Policy{
			Default: tool.ExposureDeferred,
			Discovery: tool.DiscoveryPolicy{
				MaxTools: 4,
				MaxBytes: 1024,
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()

	outcome := session.Discover("big", "a")
	if len(outcome.Evicted) == 0 || outcome.Evicted[0] != "big" {
		t.Fatalf("evicted = %v, want big dropped by byte cap", outcome.Evicted)
	}
	if outcome.Results[0].Exposed || outcome.Results[0].Reason != "over_budget" {
		t.Fatalf("big outcome = %+v, want over_budget", outcome.Results[0])
	}
	if !outcome.Results[1].Exposed {
		t.Fatalf("a outcome = %+v, want exposed", outcome.Results[1])
	}
}

func TestDynamicSession_DiscoveryIdleEviction(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("a", "1")}}},
		tool.WithDynamic(tool.Policy{
			Default:   tool.ExposureDeferred,
			Discovery: tool.DiscoveryPolicy{IdleRounds: 1},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()

	session.Discover("a")
	session.AdvanceTurn()
	if !contains(definitionNames(session.Definitions()), "a") {
		t.Fatal("discovered tool must survive one idle round")
	}

	session.RecordCall(message.ToolCall{ID: "c1", Name: "a", Arguments: []byte(`{}`)})
	session.AdvanceTurn()
	if !contains(definitionNames(session.Definitions()), "a") {
		t.Fatal("used tool must be refreshed past the idle horizon")
	}

	session.AdvanceTurn()
	if contains(definitionNames(session.Definitions()), "a") {
		t.Fatal("unused tool must expire after the idle horizon")
	}
}

func TestDynamicSession_DiscoveryIgnoresAlwaysAndHidden(t *testing.T) {
	assembly, _ := dynamicAssembly(t, "always", "deferred", "hidden")
	session := assembly.NewSession()

	outcome := session.Discover("always", "hidden", "deferred")
	if len(outcome.Results) != 3 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Results[0].Exposed || outcome.Results[0].Reason != "not_discoverable" {
		t.Fatalf("always outcome = %+v, want not_discoverable", outcome.Results[0])
	}
	if outcome.Results[1].Exposed || outcome.Results[1].Reason != "not_discoverable" {
		t.Fatalf("hidden outcome = %+v, want not_discoverable", outcome.Results[1])
	}
	if !outcome.Results[2].Exposed {
		t.Fatalf("deferred outcome = %+v, want exposed", outcome.Results[2])
	}
	names := definitionNames(session.Definitions())
	if contains(names, "hidden") {
		t.Fatalf("hidden tool leaked into Definitions: %v", names)
	}
}

func TestDynamicSession_DiscoveryStaleCatalogEntryDropped(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("a", "1")}}},
		tool.WithDynamic(tool.Policy{Default: tool.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	session.Discover("a")
	if !contains(definitionNames(session.Definitions()), "a") {
		t.Fatal("tool missing before removal")
	}
	assembly.Catalog().(*tool.Registry).Remove("a")
	if contains(definitionNames(session.Definitions()), "a") {
		t.Fatal("removed tool still visible")
	}
}

func TestDiscoverWithoutSessionContextIsSafe(t *testing.T) {
	_, err := (&tool.SearchTool{}).Execute(context.Background(), `{"query":"x"}`)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Execute without session error = %v, want NotAvailable", err)
	}
}

func TestDynamicSession_ToolSearchSurvivesBudgetPruning(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			funcTool("a", "1"), funcTool("b", "2"), funcTool("c", "3"),
		}}},
		tool.WithDynamic(tool.Policy{
			Default: tool.ExposureAlways,
			Budget:  tool.Budget{MaxDefinitions: 1},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	names := definitionNames(assembly.NewSession().Definitions())
	if len(names) != 1 || names[0] != tool.ToolName {
		t.Fatalf("Definitions = %v, want only tool_search", names)
	}
}

func TestDynamicSession_DiscoveryDirectSurvivesRecencyTie(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			funcTool("keep", "1"), funcTool("drop", "2"),
		}}},
		tool.WithDynamic(tool.Policy{
			Default: tool.ExposureDeferred,
			Exposures: map[string]tool.Exposure{
				"keep": tool.ExposureDirect,
				"drop": tool.ExposureDeferred,
			},
			Discovery: tool.DiscoveryPolicy{MaxTools: 1},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	outcome := assembly.NewSession().Discover("keep", "drop")
	if len(outcome.Results) != 2 ||
		!outcome.Results[0].Exposed || outcome.Results[1].Exposed {
		t.Fatalf("Discover outcome = %+v, want keep exposed, drop evicted", outcome)
	}
	if len(outcome.Evicted) != 1 || outcome.Evicted[0] != "drop" {
		t.Fatalf("evicted = %v, want [drop]", outcome.Evicted)
	}
}

func TestDynamicSession_LegacyRetentionSeedsIdleRounds(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("a", "1")}}},
		tool.WithDynamic(tool.Policy{
			Default:           tool.ExposureDeferred,
			SelectedRetention: 2,
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	session.Discover("a")
	for i := 0; i < 2; i++ {
		session.AdvanceTurn()
		if !contains(definitionNames(session.Definitions()), "a") {
			t.Fatalf("tool vanished after %d idle rounds (legacy retention 2)", i+1)
		}
	}
	session.AdvanceTurn()
	if contains(definitionNames(session.Definitions()), "a") {
		t.Fatal("tool must expire after legacy retention rounds")
	}
}

func TestDynamicSession_PerRoundBudgetPrefersMRUPoolEntries(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			funcTool("a", "1"), funcTool("b", "2"),
		}}},
		tool.WithDynamic(tool.Policy{
			Default: tool.ExposureDeferred,
			Budget:  tool.Budget{MaxDefinitions: 2},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	session.Discover("a")
	session.Discover("b")
	names := definitionNames(session.Definitions())
	if !contains(names, "b") || contains(names, "a") {
		t.Fatalf("Definitions = %v, want only the MRU pool entry b (plus tool_search)", names)
	}
	if !contains(names, tool.ToolName) {
		t.Fatalf("Definitions = %v, want tool_search", names)
	}
}

func TestSearchTool_ReportsLoadFailures(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{tooltest.LazySource(tooltest.LazyTool("broken", func(context.Context) (tool.Tool, error) {
			return nil, errors.New("load boom")
		}))},
		tool.WithDynamic(tool.Policy{Default: tool.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	ctx := tool.WithSession(context.Background(), session)
	search, _ := assembly.Catalog().Get(tool.ToolName)
	out, err := search.Execute(ctx, `{"query":"broken"}`)
	if err != nil {
		t.Fatalf("Execute tool_search: %v", err)
	}
	if !strings.Contains(out, `"failed":[{"name":"broken","reason":"load_failed"}]`) {
		t.Fatalf("tool_search output = %s", out)
	}
	if contains(definitionNames(session.Definitions()), "broken") {
		t.Fatal("failed lazy tool must not be exposed")
	}
}
