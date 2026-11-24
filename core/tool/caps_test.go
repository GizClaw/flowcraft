package tool_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
)

type tree struct{ root string }
type starter struct{ shell string }

var (
	treeSlot    = tool.NewSlot[*tree]("test.tree")
	starterSlot = tool.NewSlot[*starter]("test.starter")
	missingSlot = tool.NewSlot[int]("test.missing")
)

func filled() *tool.CapSet {
	set := tool.NewCapSet()
	treeSlot.Provide(set, &tree{root: "/srv"})
	starterSlot.Provide(set, &starter{shell: "/bin/sh"})
	return set
}

func TestSlotRoundTripsItsOwnType(t *testing.T) {
	view := filled().View().Narrow(treeSlot, starterSlot)

	got, ok := treeSlot.Get(view)
	if !ok || got.root != "/srv" {
		t.Fatalf("tree = %#v, ok = %v", got, ok)
	}
	if _, ok := starterSlot.Get(view); !ok {
		t.Fatalf("starter missing")
	}
}

// TestUndeclaredSlotReadsZero is the whole point of the pocket: the session holds a
// capability, the tool did not ask for it, and it must not be reachable.
func TestUndeclaredSlotReadsZero(t *testing.T) {
	view := filled().View().Narrow(treeSlot)

	if _, ok := starterSlot.Get(view); ok {
		t.Fatalf("starter readable without being declared")
	}
	if got, ok := starterSlot.Get(view); got != nil || ok {
		t.Fatalf("undeclared slot = %#v, ok = %v, want zero", got, ok)
	}
}

func TestNarrowToNothingReadsNothing(t *testing.T) {
	view := filled().View().Narrow()
	if view.Len() != 0 {
		t.Fatalf("Len = %d, want 0", view.Len())
	}
	if _, ok := treeSlot.Get(view); ok {
		t.Fatalf("declaring no needs still read a slot")
	}
}

func TestUnnarrowedViewCarriesEverything(t *testing.T) {
	view := filled().View()
	if view.Len() != 2 {
		t.Fatalf("Len = %d, want 2", view.Len())
	}
	if _, ok := treeSlot.Get(view); !ok {
		t.Fatalf("unrestricted view hid a provided slot")
	}
}

func TestGetOnAnUnfilledSlotIsZero(t *testing.T) {
	view := filled().View().Narrow(missingSlot)
	if got, ok := missingSlot.Get(view); got != 0 || ok {
		t.Fatalf("unfilled slot = %v, ok = %v", got, ok)
	}
}

// TestWrongTypeIsZeroNotAPanic covers the one way a slot name can be reused with a
// different type: the read fails instead of taking the process down.
func TestWrongTypeIsZeroNotAPanic(t *testing.T) {
	set := tool.NewCapSet()
	intSlot := tool.NewSlot[int]("test.tree")
	intSlot.Provide(set, 7)

	if got, ok := treeSlot.Get(set.View()); got != nil || ok {
		t.Fatalf("mismatched type = %#v, ok = %v, want zero", got, ok)
	}
}

func TestMissingListsUnsatisfiedNeedsSorted(t *testing.T) {
	set := tool.NewCapSet()
	treeSlot.Provide(set, &tree{})

	missing := set.Missing(starterSlot, treeSlot, missingSlot)
	if len(missing) != 2 || missing[0] != "test.missing" || missing[1] != "test.starter" {
		t.Fatalf("missing = %v, want [test.missing test.starter]", missing)
	}
	if got := set.Missing(treeSlot); len(got) != 0 {
		t.Fatalf("satisfied need reported missing: %v", got)
	}
}

// TestZeroValuesAreEmpty keeps the zero Binding safe to read: a host that forgot to
// fill the pocket must not panic a tool.
func TestZeroValuesAreEmpty(t *testing.T) {
	var set *tool.CapSet
	if set.Has(treeSlot) {
		t.Fatalf("nil set has a slot")
	}
	if set.Missing(treeSlot) == nil {
		t.Fatalf("nil set satisfied its needs")
	}
	var view tool.CapView
	if view.Len() != 0 {
		t.Fatalf("zero view Len = %d", view.Len())
	}
	if _, ok := treeSlot.Get(view.Narrow(treeSlot)); ok {
		t.Fatalf("zero view read a slot")
	}
	// Providing into a nil set is a no-op rather than a panic: a tool's tests build
	// Bindings by hand.
	treeSlot.Provide(nil, &tree{})
}

// TestWithDoesNotAllocate pins the per-call path: dispatch narrows every call, so
// narrowing to a precomputed allowance must not allocate.
func TestWithDoesNotAllocate(t *testing.T) {
	view := filled().View()
	allow := tool.AllowOf(treeSlot, starterSlot)
	if allocs := testing.AllocsPerRun(100, func() { _ = view.With(allow) }); allocs != 0 {
		t.Fatalf("With allocates %v times, want 0", allocs)
	}
}

// TestNarrowAllocatesOnlyTheAllowance keeps the convenience path honest: building the
// declaration list on the spot costs the variadic slice and the map, and nothing per
// capability. With is what dispatch calls.
func TestNarrowAllocatesOnlyTheAllowance(t *testing.T) {
	view := filled().View()
	if allocs := testing.AllocsPerRun(100, func() { _ = view.Narrow(treeSlot, starterSlot) }); allocs > 4 {
		t.Fatalf("Narrow allocates %v times, want the allowance only", allocs)
	}
}

// TestZeroAllowReadsNothing is the fail-closed direction: a View narrowed by an
// allowance that failed to build must not hand out the session's capabilities.
func TestZeroAllowReadsNothing(t *testing.T) {
	view := filled().View().With(tool.Allow{})
	if view.Len() != 0 {
		t.Fatalf("Len = %d, want 0", view.Len())
	}
	if _, ok := treeSlot.Get(view); ok {
		t.Fatalf("zero allowance read a slot")
	}
}

func TestAllowOfZeroNeedsIsNotEmpty(t *testing.T) {
	view := filled().View().With(tool.AllowOf())
	if _, ok := treeSlot.Get(view); ok {
		t.Fatalf("empty declaration read a slot")
	}
}
