package dynamic

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
)

func def(name string) message.Definition {
	return message.Definition{
		Name:        name,
		Description: "tool " + name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func candidates(names ...string) []candidate {
	out := make([]candidate, 0, len(names))
	for _, name := range names {
		out = append(out, candidate{name: name, def: def(name), exp: ExposureDeferred})
	}
	return out
}

func withExposures(cands []candidate, exps map[string]Exposure) []candidate {
	for i := range cands {
		if e, ok := exps[cands[i].name]; ok {
			cands[i].exp = e
		}
	}
	return cands
}

func visibleNames(cands []candidate, st stateSnapshot, policy Policy) []string {
	out := visibleCandidates(cands, st, policy)
	names := make([]string, 0, len(out))
	for _, c := range out {
		names = append(names, c.name)
	}
	return names
}

func TestVisibility_ExposureBaselines(t *testing.T) {
	policy := DefaultPolicy()
	policy.Default = ExposureHidden
	policy.Budget = Budget{MaxDefinitions: 100, MaxBytes: 1 << 20}
	policy.SelectedRetention = 3
	policy.RecentWindow = 3
	st := newState().snapshot()

	cands := withExposures(candidates("always_a", "direct_b", "deferred_c", "hidden_d"), map[string]Exposure{
		"always_a": ExposureAlways, "direct_b": ExposureDirect,
		"deferred_c": ExposureDeferred, "hidden_d": ExposureHidden,
	})

	got := visibleNames(cands, st, policy)
	want := []string{"always_a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("baseline visible = %v, want %v", got, want)
	}
}

func TestVisibility_DirectRequiredRecent(t *testing.T) {
	policy := DefaultPolicy()
	policy.Default = ExposureHidden
	policy.Budget = Budget{MaxDefinitions: 100, MaxBytes: 1 << 20}
	policy.SelectedRetention = 3
	policy.RecentWindow = 3

	cands := withExposures(candidates("d1", "d2", "d3"), map[string]Exposure{
		"d1": ExposureDirect, "d2": ExposureDirect, "d3": ExposureDirect,
	})

	base := newState()
	base.turn = 10
	base.require("d1")
	base.selectNames([]string{"d2"}, 3)
	base.recordCall("d3", 3)

	got := visibleNames(cands, base.snapshot(), policy)
	want := []string{"d1", "d2", "d3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("direct visible = %v, want %v", got, want)
	}
}

func TestVisibility_DeferredRequiresSelection(t *testing.T) {
	policy := DefaultPolicy()
	policy.Default = ExposureHidden
	policy.Budget = Budget{MaxDefinitions: 100, MaxBytes: 1 << 20}
	policy.SelectedRetention = 2

	cands := withExposures(candidates("def"), map[string]Exposure{"def": ExposureDeferred})

	if got := visibleNames(cands, newState().snapshot(), policy); len(got) != 0 {
		t.Errorf("unselected deferred tool visible: %v", got)
	}

	st := newState()
	st.selectNames([]string{"def"}, 2)
	if got := visibleNames(cands, st.snapshot(), policy); !reflect.DeepEqual(got, []string{"def"}) {
		t.Errorf("selected deferred tool not visible: %v", got)
	}

	st.advanceTurn(10)
	if got := visibleNames(cands, st.snapshot(), policy); !reflect.DeepEqual(got, []string{"def"}) {
		t.Errorf("deferred tool expired one round early: %v", got)
	}
	st.advanceTurn(10)
	if got := visibleNames(cands, st.snapshot(), policy); len(got) != 0 {
		t.Errorf("deferred tool visible after retention expiry: %v", got)
	}
}

func TestVisibility_HiddenOnlyRequired(t *testing.T) {
	policy := DefaultPolicy()
	policy.Default = ExposureHidden
	policy.Budget = Budget{MaxDefinitions: 100, MaxBytes: 1 << 20}

	cands := withExposures(candidates("h"), map[string]Exposure{"h": ExposureHidden})
	st := newState()
	st.recordCall("h", 5)
	if got := visibleNames(cands, st.snapshot(), policy); len(got) != 0 {
		t.Errorf("hidden tool visible after use: %v", got)
	}
	st.require("h")
	if got := visibleNames(cands, st.snapshot(), policy); !reflect.DeepEqual(got, []string{"h"}) {
		t.Errorf("hidden tool not visible when required: %v", got)
	}
}

func TestVisibility_BudgetPrunesDeterministically(t *testing.T) {
	policy := DefaultPolicy()
	policy.Default = ExposureDeferred
	policy.Budget = Budget{MaxDefinitions: 3, MaxBytes: 1 << 20}
	policy.SelectedRetention = 3
	policy.RecentWindow = 10

	cands := withExposures(candidates("a", "b", "c", "d"), map[string]Exposure{
		"a": ExposureAlways, "b": ExposureAlways, "c": ExposureAlways, "d": ExposureAlways,
	})
	st := newState().snapshot()
	got := visibleNames(cands, st, policy)
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("pruned = %v, want [a b c]", got)
	}
	// Deterministic: same input, same output.
	if again := visibleNames(cands, st, policy); !reflect.DeepEqual(got, again) {
		t.Errorf("visible set is not deterministic: %v vs %v", got, again)
	}
}

func TestVisibility_ByteBudgetEvictsLargestLast(t *testing.T) {
	policy := DefaultPolicy()
	policy.Default = ExposureAlways
	policy.Budget = Budget{MaxDefinitions: 100, MaxBytes: 200}

	big := def("big")
	big.Description = string(make([]byte, 500))
	small := def("small")
	small.Description = "x"

	cands := []candidate{
		{name: "big", def: big, exp: ExposureAlways},
		{name: "small", def: small, exp: ExposureAlways},
	}
	got := visibleNames(cands, newState().snapshot(), policy)
	if !reflect.DeepEqual(got, []string{"big"}) {
		t.Errorf("byte budget result = %v, want only big (first never evicted)", got)
	}
}
