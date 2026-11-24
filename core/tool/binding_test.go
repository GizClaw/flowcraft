package tool_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

type handle struct{ name string }

var (
	handleSlot = tool.NewSlot[*handle]("test.handle")
	otherSlot  = tool.NewSlot[*handle]("test.other")
)

// TestZeroBindingIsInvalid pins the one invariant the host is responsible for: "no
// binding" must never read as "unrestricted". Dispatch refuses it, and the refusal is
// only correct if the zero value reports itself.
func TestZeroBindingIsInvalid(t *testing.T) {
	var b tool.Binding
	if b.Valid() {
		t.Fatal("the zero Binding reported itself valid")
	}
	if _, ok := handleSlot.Get(b.Caps); ok {
		t.Fatal("the zero Binding handed out a capability")
	}
	if b.View != nil {
		t.Fatal("the zero Binding carries a view")
	}
}

func TestBindCarriesTheSessionAndItsGrant(t *testing.T) {
	grant := policy.Grant{Sandbox: "strict"}
	b := tool.Bind(tooltest.Session("s1"), grant)

	if !b.Valid() {
		t.Fatal("Bind produced an invalid binding")
	}
	if b.Session.ID() != "s1" {
		t.Fatalf("session = %q", b.Session.ID())
	}
	if b.Grant.Sandbox != "strict" {
		t.Fatal("the grant was not carried")
	}
	if b.View != nil {
		t.Fatal("Bind wired a view nobody supplied")
	}
}

func TestWithCapsAndNarrow(t *testing.T) {
	set := tool.NewCapSet()
	handleSlot.Provide(set, &handle{name: "first"})
	otherSlot.Provide(set, &handle{name: "second"})

	b := tool.Bind(tooltest.Session("s1"), policy.Grant{}).WithCaps(set).Narrow(tool.AllowOf(handleSlot))

	got, ok := handleSlot.Get(b.Caps)
	if !ok || got.name != "first" {
		t.Fatalf("declared slot = %#v, ok = %v", got, ok)
	}
	if _, ok := otherSlot.Get(b.Caps); ok {
		t.Fatal("a slot outside the allowance was readable")
	}
	// Narrowing is a view, not a rewrite: the session still holds what it held.
	if _, ok := otherSlot.Get(set.View()); !ok {
		t.Fatal("narrowing dropped a capability from the session's set")
	}
}

// TestNarrowDoesNotMutateTheHostsBinding keeps the narrowing chain-safe: middleware
// passes the binding down by value, so a narrow at one stage must not shorten what the
// next stage can see.
func TestNarrowDoesNotMutateTheHostsBinding(t *testing.T) {
	set := tool.NewCapSet()
	handleSlot.Provide(set, &handle{name: "first"})
	b := tool.Bind(tooltest.Session("s1"), policy.Grant{}).WithCaps(set)

	narrowed := b.Narrow(tool.AllowOf())
	if _, ok := handleSlot.Get(narrowed.Caps); ok {
		t.Fatal("the empty allowance kept a capability")
	}
	if _, ok := handleSlot.Get(b.Caps); !ok {
		t.Fatal("narrowing mutated the caller's binding")
	}
}

func TestWithCapsViewTakesAPreNarrowedPocket(t *testing.T) {
	set := tool.NewCapSet()
	handleSlot.Provide(set, &handle{name: "first"})
	view := set.View().Narrow(handleSlot)

	b := tool.Bind(tooltest.Session("s1"), policy.Grant{}).WithCapsView(view)
	if _, ok := handleSlot.Get(b.Caps); !ok {
		t.Fatal("the pre-narrowed pocket lost its declared slot")
	}
}
