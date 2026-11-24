package identity_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/identity"
)

func fullRun() identity.Run {
	return identity.Run{
		Tenant:    "tenant-1",
		Agent:     "assistant",
		Authority: "authority-1",
		SessionID: "session-1",
		Bind:      "bind-1",
		TaskID:    "task-1",
		Ordinal:   3,
		ChainID:   "chain-1",
		Depth:     1,
	}
}

// TestPairsIsOrderedAndOmitsUnset is the contract every renderer depends on: the
// keys come in one order, and a fact that is unset is absent rather than empty — an
// empty string in a dashboard is a filter that matches everything.
func TestPairsIsOrderedAndOmitsUnset(t *testing.T) {
	pairs := fullRun().Pairs()
	want := [][2]string{
		{identity.AttrTenantID, "tenant-1"},
		{identity.AttrAgentName, "assistant"},
		{identity.AttrAuthorityID, "authority-1"},
		{identity.AttrSessionID, "session-1"},
		{identity.AttrBindID, "bind-1"},
		{identity.AttrTaskID, "task-1"},
		{identity.AttrTurn, "3"},
		{identity.AttrChainID, "chain-1"},
		{identity.AttrChainDepth, "1"},
	}
	if len(pairs) != len(want) {
		t.Fatalf("pairs = %v, want %v", pairs, want)
	}
	for i := range want {
		if pairs[i] != want[i] {
			t.Fatalf("pair %d = %v, want %v", i, pairs[i], want[i])
		}
	}
}

func TestPairsOmitsWhatIsUnset(t *testing.T) {
	pairs := identity.Run{Agent: "assistant", SessionID: "session-1"}.Pairs()
	if len(pairs) != 2 {
		t.Fatalf("pairs = %v, want only the two set facts", pairs)
	}
	for _, pair := range pairs {
		if pair[1] == "" {
			t.Fatalf("pair %v carries an empty value", pair)
		}
	}
	// A zero ordinal and a zero depth are "unset", not "zero": a root caller has no
	// depth to report.
	for _, pair := range pairs {
		if pair[0] == identity.AttrTurn || pair[0] == identity.AttrChainDepth {
			t.Fatalf("an unset numeric fact was rendered: %v", pair)
		}
	}
}

func TestLookupAndAttach(t *testing.T) {
	ctx := context.Background()
	if _, ok := identity.Lookup(ctx); ok {
		t.Fatal("a bare context reported an identity")
	}

	run := fullRun()
	attached, ok := identity.Lookup(identity.Attach(ctx, run))
	if !ok || attached != run {
		t.Fatalf("Lookup = %+v, %v", attached, ok)
	}
	if run.IsZero() {
		t.Fatal("a populated run reported zero")
	}
	if !(identity.Run{}).IsZero() {
		t.Fatal("the zero value did not report zero")
	}
}

// TestAttributeNamesAreStable pins the spelling. These strings are what every
// dashboard, alert and log query filters on, so a rename is a breaking change and
// should be an explicit one.
func TestAttributeNamesAreStable(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{identity.AttrTenantID, "tenant.id"},
		{identity.AttrAgentName, "agent.name"},
		{identity.AttrAuthorityID, "agent.authority.id"},
		{identity.AttrSessionID, "session.id"},
		{identity.AttrBindID, "session.bind.id"},
		{identity.AttrTaskID, "task.id"},
		{identity.AttrTurn, "turn"},
		{identity.AttrChainID, "chain.id"},
		{identity.AttrChainDepth, "chain.depth"},
	} {
		if tc.got != tc.want {
			t.Errorf("attribute name = %q, want %q", tc.got, tc.want)
		}
	}
}
