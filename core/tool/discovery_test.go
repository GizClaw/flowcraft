package tool_test

import (
	"context"
	"errors"
	"fmt"
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
	if rendered := jsonOf(t, out); !strings.Contains(rendered, `"failed":[{"name":"broken","reason":"load_failed"}]`) {
		t.Fatalf("tool_search output = %s", rendered)
	}
	if contains(definitionNames(session.Definitions()), "broken") {
		t.Fatal("failed lazy tool must not be exposed")
	}
}

// definitionSize mirrors tool.definitionBytes so a test can size a tool
// exactly.
func definitionSize(d message.ToolDefinition) int {
	return len(d.Name) + len(d.Description) + len(d.InputSchema) + 32
}

// sizedFuncTool builds a tool whose serialized definition is size bytes.
func sizedFuncTool(name string, size int) tool.Tool {
	const schema = `{"type":"object"}`
	pad := size - len(name) - len(schema) - 32
	if pad < 0 {
		panic("sizedFuncTool: size too small for " + name)
	}
	return tooltest.FuncTool(name, strings.Repeat("x", pad),
		func(context.Context, string) (string, error) { return name, nil })
}

// TestDynamicSession_DiscoverReportsPerRoundVisibility pins the contract
// from issue #558: Discover may only report a name as exposed when the
// next Definitions call really sends it. The byte numbers mirror the
// host that reported the trap.
func TestDynamicSession_DiscoverReportsPerRoundVisibility(t *testing.T) {
	const alwaysBytes = 11260 // always-visible baseline, incl. tool_search
	order := []string{
		"generate_video", "generate_image", "mcp_0", "mcp_1",
		"mcp_2", "mcp_3", "create_agent", "web_fetch",
	}
	sizes := map[string]int{
		"generate_video": 2724,
		"generate_image": 2439,
		"mcp_0":          985,
		"mcp_1":          985,
		"mcp_2":          985,
		"mcp_3":          984,
		"create_agent":   1450,
		"web_fetch":      773,
	}

	exposures := make(map[string]tool.Exposure, 15)
	tools := make([]tool.Tool, 0, 15+len(order))
	baseTotal := alwaysBytes - definitionSize(tool.NewSearchTool().Definition())
	baseEach := baseTotal / 15
	for i := 0; i < 15; i++ {
		size := baseEach
		if i == 14 {
			size = baseTotal - baseEach*14
		}
		name := fmt.Sprintf("base_%02d", i)
		exposures[name] = tool.ExposureAlways
		tools = append(tools, sizedFuncTool(name, size))
	}
	for _, name := range order {
		tools = append(tools, sizedFuncTool(name, sizes[name]))
	}

	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: tools}},
		tool.WithDynamic(tool.Policy{
			Default:   tool.ExposureDeferred,
			Exposures: exposures,
			Budget:    tool.DefaultBudget,
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	outcome := session.Discover(order...)

	visible := make(map[string]bool)
	for _, def := range session.Definitions() {
		visible[def.Name] = true
	}
	var dropped []string
	for _, r := range outcome.Results {
		switch {
		case r.Exposed && !visible[r.Name]:
			t.Errorf("%s reported exposed but Definitions() drops it", r.Name)
		case !r.Exposed && visible[r.Name]:
			t.Errorf("%s reported %q but Definitions() sends it", r.Name, r.Reason)
		case !r.Exposed && r.Reason == "":
			t.Errorf("%s is not exposed without a reason", r.Name)
		}
		if !r.Exposed {
			dropped = append(dropped, r.Name+":"+r.Reason)
		}
	}
	if len(dropped) == 0 {
		t.Fatalf("setup no longer exercises the visible budget: %+v", outcome.Results)
	}
	// The first hit is the best-ranked one: it must survive the cut.
	if !visible["generate_video"] {
		t.Errorf("best-ranked hit lost the visible budget: dropped %v", dropped)
	}
	for _, want := range dropped {
		if !strings.HasSuffix(want, ":visible_budget") {
			t.Errorf("dropped %s without the visible_budget reason", want)
		}
	}
	t.Logf("dropped %v", dropped)
}

// TestSearchTool_ReportsVisibleBudgetHits checks the model-facing
// payload: a hit that is pooled but loses the per-round budget comes
// back under failed with reason visible_budget instead of being listed
// as exposed.
func TestSearchTool_ReportsVisibleBudgetHits(t *testing.T) {
	const budgetBytes = 4096
	searchBytes := definitionSize(tool.NewSearchTool().Definition())
	tools := []tool.Tool{
		sizedFuncTool("base", budgetBytes-searchBytes-800),
		sizedFuncTool("aaa_hit", 700),
		sizedFuncTool("bbb_hit", 700),
	}
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: tools}},
		tool.WithDynamic(tool.Policy{
			Default:   tool.ExposureDeferred,
			Exposures: map[string]tool.Exposure{"base": tool.ExposureAlways},
			Budget:    tool.Budget{MaxDefinitions: 8, MaxBytes: budgetBytes},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	ctx := tool.WithSession(context.Background(), session)
	search, _ := assembly.Catalog().Get(tool.ToolName)
	out, err := search.Execute(ctx, `{"query":"hit"}`)
	if err != nil {
		t.Fatalf("Execute tool_search: %v", err)
	}
	rendered := jsonOf(t, out)
	if !strings.Contains(rendered, `"exposed":["aaa_hit"]`) {
		t.Fatalf("tool_search output = %s, want only aaa_hit exposed", rendered)
	}
	if !strings.Contains(rendered, `{"name":"bbb_hit","reason":"visible_budget"}`) {
		t.Fatalf("tool_search output = %s, want bbb_hit reported as visible_budget", rendered)
	}
	names := definitionNames(session.Definitions())
	if !contains(names, "aaa_hit") || contains(names, "bbb_hit") {
		t.Fatalf("Definitions = %v, want aaa_hit only", names)
	}
}

// TestDynamicSession_DiscoveryDefaultsFollowVisibleBudget keeps the two
// budgets aligned: a host that raises only budget.* must not keep a pool
// stuck at DefaultBudget, which silently withheld discovered tools.
func TestDynamicSession_DiscoveryDefaultsFollowVisibleBudget(t *testing.T) {
	const (
		count    = 20
		toolSize = 2048 // 40 KiB total: fits a 64 KiB budget, not the 16 KiB default
	)
	tools := make([]tool.Tool, 0, count)
	names := make([]string, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("mcp_%02d", i)
		names = append(names, name)
		tools = append(tools, sizedFuncTool(name, toolSize))
	}

	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: tools}},
		tool.WithDynamic(tool.Policy{
			Default: tool.ExposureDeferred,
			Budget:  tool.Budget{MaxDefinitions: 64, MaxBytes: 64 * 1024},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	outcome := session.Discover(names...)
	if len(outcome.Evicted) != 0 {
		t.Fatalf("evicted %v, want the pool to follow budget.max_bytes", outcome.Evicted)
	}
	visible := definitionNames(session.Definitions())
	for _, name := range names {
		if !contains(visible, name) {
			t.Fatalf("%s missing from Definitions = %v", name, visible)
		}
	}
}
