package projection_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/projection"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// catalogOf builds the catalog the projection reads: a plain registry, which also
// answers DefinitionsEpoch, so the second Definitions call is a cache hit.
func catalogOf(t *testing.T, names ...string) *tool.Registry {
	t.Helper()
	registry, err := tool.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, name := range names {
		def := message.ToolDefinition{
			Name:        name,
			Description: "does " + name,
			InputSchema: []byte(`{"type":"object"}`),
		}
		if err := registry.Add(tool.TextTool(def, func(context.Context, string) (string, error) {
			return name, nil
		})); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
	}
	return registry
}

func newProjection(t *testing.T, policy projection.Policy, names ...string) (*projection.Projection, *projection.View) {
	t.Helper()
	p, err := projection.New(catalogOf(t, names...), policy)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, p.NewView()
}

// jsonText is the answer's payload: tool_search replies with a structured part, not
// prose, so Content.Text would be empty by design.
func jsonText(t *testing.T, content message.Content) string {
	t.Helper()
	for _, part := range content.Parts {
		if data, ok := part.(message.DataPart); ok {
			return string(data.Value)
		}
	}
	t.Fatalf("content has no data part: %+v", content)
	return ""
}

func names(defs []message.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		out = append(out, def.Name)
	}
	return out
}

func TestNewValidatesThePolicy(t *testing.T) {
	catalog := catalogOf(t, "a")
	for name, policy := range map[string]projection.Policy{
		"bad default exposure": {Default: "sometimes"},
		"tool_search not always": {
			Default:   projection.ExposureDeferred,
			Exposures: map[string]projection.Exposure{projection.ToolName: projection.ExposureDeferred},
		},
		"negative pool": {Discovery: projection.DiscoveryPolicy{MaxTools: -1}},
	} {
		if _, err := projection.New(catalog, policy); err == nil {
			t.Fatalf("%s: New accepted an invalid policy", name)
		}
	}
}

// TestStaticShowsEverything pins the no-discovery shape: a host that does not want
// tool_search gets a view with no state at all.
func TestStaticShowsEverything(t *testing.T) {
	catalog := catalogOf(t, "a", "b")
	view := projection.Static(catalog)

	if got := names(view.Definitions()); len(got) != 2 {
		t.Fatalf("definitions = %v, want two", got)
	}
	if !view.Visible("a") || view.Visible("missing") {
		t.Fatal("Visible disagreed with the catalog")
	}
	view.RecordCall(message.ToolCall{Name: "a"}) // no state: must not panic
}

// TestAlwaysIsVisibleWithoutDiscovery is the baseline: the policy's exposures decide
// what a fresh session sees, before anything is discovered.
func TestAlwaysIsVisibleWithoutDiscovery(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureDeferred,
		Exposures: map[string]projection.Exposure{"base": projection.ExposureAlways},
	}, "base", "hidden-until-found")

	if got := names(view.Definitions()); len(got) != 1 || got[0] != "base" {
		t.Fatalf("definitions = %v, want [base]", got)
	}
	if view.Visible("hidden-until-found") {
		t.Fatal("a deferred tool was visible before discovery")
	}
}

func TestRequiredSurfacesAHiddenTool(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureHidden,
		Exposures: map[string]projection.Exposure{"needed": projection.ExposureHidden},
	}, "needed", "other")

	view.Require("needed")
	if got := names(view.Definitions()); len(got) != 1 || got[0] != "needed" {
		t.Fatalf("definitions = %v, want [needed]", got)
	}
	if view.Visible("other") {
		t.Fatal("an unrequired hidden tool was visible")
	}
}

func TestDefinitionsAreSortedByName(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default: projection.ExposureAlways,
	}, "c", "a", "b")

	got := names(view.Definitions())
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("definitions = %v, want a,b,c", got)
	}
}

func TestDiscoverReportsWhatTheNextRoundSends(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureDeferred,
		Exposures: map[string]projection.Exposure{projection.ToolName: projection.ExposureAlways},
	}, "found", projection.ToolName)

	outcome := view.Discover("found", "missing", projection.ToolName)
	byName := map[string]projection.DiscoverResult{}
	for _, result := range outcome.Results {
		byName[result.Name] = result
	}
	if !byName["found"].Exposed {
		t.Fatalf("discovered tool not exposed: %+v", byName["found"])
	}
	if byName["missing"].Reason != "unknown" {
		t.Fatalf("unknown reason = %q", byName["missing"].Reason)
	}
	if byName[projection.ToolName].Reason != "not_discoverable" {
		t.Fatalf("always-visible reason = %q", byName[projection.ToolName].Reason)
	}
	// The report must agree with what is actually sent.
	if got := names(view.Definitions()); len(got) != 2 {
		t.Fatalf("definitions = %v, want the discovered tool and tool_search", got)
	}
}

// TestDiscoveryPoolEvictsLeastRecentlyUsed pins the pool bound: the visible set is
// capped, and what falls out is what was used longest ago.
func TestDiscoveryPoolEvictsLeastRecentlyUsed(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureDeferred,
		Budget:    projection.Budget{MaxDefinitions: 10, MaxBytes: 1 << 20},
		Discovery: projection.DiscoveryPolicy{MaxTools: 2, MaxBytes: 1 << 20, IdleRounds: 10},
	}, "a", "b", "c")

	view.Discover("a")
	view.Discover("b")
	outcome := view.Discover("c")

	if len(outcome.Evicted) != 1 || outcome.Evicted[0] != "a" {
		t.Fatalf("evicted = %v, want [a]", outcome.Evicted)
	}
	got := names(view.Definitions())
	if strings.Join(got, ",") != "b,c" {
		t.Fatalf("definitions = %v, want b,c", got)
	}
}

// TestRecordCallKeepsADiscoveredToolWarm is what keeps a tool the model is actively
// using from falling out of the pool.
func TestRecordCallKeepsADiscoveredToolWarm(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureDeferred,
		Budget:    projection.Budget{MaxDefinitions: 10, MaxBytes: 1 << 20},
		Discovery: projection.DiscoveryPolicy{MaxTools: 2, MaxBytes: 1 << 20, IdleRounds: 10},
	}, "a", "b", "c")

	view.Discover("a")
	view.Discover("b")
	view.RecordCall(message.ToolCall{Name: "a"})
	if outcome := view.Discover("c"); len(outcome.Evicted) != 1 || outcome.Evicted[0] != "b" {
		t.Fatalf("evicted = %v, want [b]", outcome.Evicted)
	}
}

func TestRecordCallIgnoresAlwaysAndHidden(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Exposures: map[string]projection.Exposure{
			"always": projection.ExposureAlways,
			"hidden": projection.ExposureHidden,
		},
		Default: projection.ExposureDeferred,
	}, "always", "hidden")

	before := view.Epoch()
	view.RecordCall(message.ToolCall{Name: "always"})
	view.RecordCall(message.ToolCall{Name: "hidden"})
	if view.Epoch() != before {
		t.Fatal("recording an always/hidden call moved the visible-set epoch")
	}
}

// TestAdvanceRoundExpiresIdleEntries pins the idle horizon: a tool discovered long ago
// and never used stops being visible, so a long session does not accumulate a toolset.
func TestAdvanceRoundExpiresIdleEntries(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureDeferred,
		Budget:    projection.Budget{MaxDefinitions: 10, MaxBytes: 1 << 20},
		Discovery: projection.DiscoveryPolicy{MaxTools: 10, MaxBytes: 1 << 20, IdleRounds: 1},
	}, "found")

	view.Discover("found")
	if !view.Visible("found") {
		t.Fatal("discovered tool was not visible")
	}
	view.AdvanceRound()
	view.AdvanceRound()
	if view.Visible("found") {
		t.Fatal("an idle discovery did not expire")
	}
	if got := names(view.Definitions()); len(got) != 0 {
		t.Fatalf("definitions = %v, want none", got)
	}
}

// TestPerRoundBudgetCutsTheVisibleSet is the other cap: the pool remembers more than one
// round can show, and the round keeps the highest-priority candidates.
func TestPerRoundBudgetCutsTheVisibleSet(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureDeferred,
		Budget:    projection.Budget{MaxDefinitions: 1, MaxBytes: 1 << 20},
		Discovery: projection.DiscoveryPolicy{MaxTools: 10, MaxBytes: 1 << 20, IdleRounds: 10},
	}, "a", "b")

	outcome := view.Discover("a", "b")
	if !outcome.Results[0].Exposed {
		t.Fatalf("the best-ranked hit was not exposed: %+v", outcome.Results[0])
	}
	if outcome.Results[1].Exposed || outcome.Results[1].Reason != "visible_budget" {
		t.Fatalf("second hit = %+v, want visible_budget", outcome.Results[1])
	}
}

func TestSearchRanksTheCatalogAndNeverExceedsThePool(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureDeferred,
		Discovery: projection.DiscoveryPolicy{MaxTools: 1, MaxBytes: 1 << 20, IdleRounds: 10},
	}, "read_file", "write_file", "send_email")

	hits, err := view.Search(context.Background(), "file", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want the pool cap of 1", len(hits))
	}
	if !strings.Contains(hits[0].Name, "file") {
		t.Fatalf("hit = %q, want a file tool", hits[0].Name)
	}
}

func TestSearchWithAQueryThatMatchesNothing(t *testing.T) {
	_, view := newProjection(t, projection.Policy{Default: projection.ExposureDeferred}, "a")

	hits, err := view.Search(context.Background(), "zzzz", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %v, want none", hits)
	}
}

// TestToolSearchExposesWhatItFinds is the end-to-end path: the model calls the tool, and
// the hits appear in the next round with their real definitions.
func TestToolSearchExposesWhatItFinds(t *testing.T) {
	_, view := newProjection(t, projection.Policy{
		Default:   projection.ExposureDeferred,
		Exposures: map[string]projection.Exposure{projection.ToolName: projection.ExposureAlways},
	}, "read_file", projection.ToolName)

	binding := tool.Bind(tooltest.Session("s1"), policy.Grant{}).WithView(view)
	content, err := projection.SearchTool{}.Execute(context.Background(), binding,
		`{"query":"read a file"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(jsonText(t, content), `"exposed":["read_file"]`) {
		t.Fatalf("answer = %s", jsonText(t, content))
	}
	if !view.Visible("read_file") {
		t.Fatal("the discovered tool is not visible")
	}
}

// TestToolSearchWithoutAProjectionIsUnavailable keeps the failure legible: a tool bound
// to a static view cannot discover, and says so.
func TestToolSearchWithoutAProjectionIsUnavailable(t *testing.T) {
	catalog := catalogOf(t, projection.ToolName)
	binding := tool.Bind(tooltest.Session("s1"), policy.Grant{}).
		WithView(projection.Static(catalog))

	if _, err := (projection.SearchTool{}).Execute(context.Background(), binding, `{"query":"x"}`); err == nil {
		t.Fatal("tool_search ran without a discovery projection")
	}
}

func TestToolSearchRejectsAnEmptyQuery(t *testing.T) {
	_, view := newProjection(t, projection.Policy{Default: projection.ExposureDeferred}, "a")
	binding := tool.Bind(tooltest.Session("s1"), policy.Grant{}).WithView(view)

	if _, err := (projection.SearchTool{}).Execute(context.Background(), binding, `{"query":"  "}`); err == nil {
		t.Fatal("an empty query was accepted")
	}
}
