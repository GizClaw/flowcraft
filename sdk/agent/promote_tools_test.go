package agent

import (
	"reflect"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
)

// TestPromoteAgentTools_NilCallerDepsAllocates documents the
// "no caller-deps, agent has tools" path: Run must hand the engine
// a Dependencies container even when the caller never registered
// one — otherwise engines that look up depname.ToolAllowedNames
// would never find the gate and Agent.Tools would silently leak
// back into the audit's #1 contract gap.
func TestPromoteAgentTools_NilCallerDepsAllocates(t *testing.T) {
	got := promoteAgentTools(nil, []string{"search", "fetch"})
	if got == nil {
		t.Fatal("expected an allocated Dependencies, got nil")
	}
	v, err := engine.GetDep[[]string](got, depname.ToolAllowedNames)
	if err != nil {
		t.Fatalf("ToolAllowedNames missing: %v", err)
	}
	want := []string{"search", "fetch"}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("ToolAllowedNames = %v, want %v", v, want)
	}
}

// TestPromoteAgentTools_NoToolsIsNoop documents the back-compat
// path: agents that do not opt into the policy gate (Tools nil or
// empty) get the caller's deps back unchanged. Forward-compatible
// for codebases that haven't started populating agent.Tools yet.
func TestPromoteAgentTools_NoToolsIsNoop(t *testing.T) {
	deps := engine.NewDependencies()
	deps.Set("user-key", "preserved")

	if got := promoteAgentTools(deps, nil); got != deps {
		t.Errorf("nil Tools should return caller deps verbatim, got fresh container")
	}
	if got := promoteAgentTools(deps, []string{}); got != deps {
		t.Errorf("empty Tools should return caller deps verbatim, got fresh container")
	}

	if got := promoteAgentTools(nil, nil); got != nil {
		t.Errorf("nil + nil should stay nil; got %v", got)
	}
}

// TestPromoteAgentTools_CallerSuppliedWins is the symmetric rule
// to mergeAttributes' "caller wins" policy: a power user that has
// already set depname.ToolAllowedNames on their Dependencies (e.g.
// to override the agent's claim for a single call) should NOT have
// the agent.Tools value overwrite it.
func TestPromoteAgentTools_CallerSuppliedWins(t *testing.T) {
	deps := engine.NewDependencies()
	deps.Set(depname.ToolAllowedNames, []string{"caller-override"})

	got := promoteAgentTools(deps, []string{"agent-tools"})
	if got != deps {
		t.Fatalf("caller-supplied container should be returned unchanged, got fresh container")
	}
	v, err := engine.GetDep[[]string](deps, depname.ToolAllowedNames)
	if err != nil {
		t.Fatalf("post-call lookup: %v", err)
	}
	if !reflect.DeepEqual(v, []string{"caller-override"}) {
		t.Errorf("caller value mutated: got %v want [caller-override]", v)
	}
}

// TestPromoteAgentTools_ClonesToAvoidPollution is the most
// important regression: the same Dependencies container reused
// across runs of different agents must not accumulate one agent's
// allow-list and surface it to another.
func TestPromoteAgentTools_ClonesToAvoidPollution(t *testing.T) {
	shared := engine.NewDependencies()
	shared.Set("user-key", "preserved")

	depsA := promoteAgentTools(shared, []string{"agent_a_tool"})
	depsB := promoteAgentTools(shared, []string{"agent_b_tool"})

	if depsA == shared || depsB == shared {
		t.Fatal("promoteAgentTools must clone, not mutate the caller's container")
	}
	if shared.Has(depname.ToolAllowedNames) {
		t.Error("shared (caller) container must NOT carry the promoted key")
	}

	a, _ := engine.GetDep[[]string](depsA, depname.ToolAllowedNames)
	b, _ := engine.GetDep[[]string](depsB, depname.ToolAllowedNames)
	if !reflect.DeepEqual(a, []string{"agent_a_tool"}) {
		t.Errorf("depsA leaked: got %v", a)
	}
	if !reflect.DeepEqual(b, []string{"agent_b_tool"}) {
		t.Errorf("depsB leaked: got %v", b)
	}

	// Pre-existing keys are preserved on the clones.
	for _, d := range []*engine.Dependencies{depsA, depsB} {
		v, _ := d.Get("user-key")
		if v != "preserved" {
			t.Errorf("pre-existing user key dropped from clone: got %v", v)
		}
	}
}

// TestPromoteAgentTools_DefensiveSliceCopy asserts the helper
// detaches from the caller's []string so mutating ag.Tools after
// Run returns (next call to applyOptions, etc.) cannot retroactively
// alter the gate the engine already saw.
func TestPromoteAgentTools_DefensiveSliceCopy(t *testing.T) {
	original := []string{"search", "fetch"}
	deps := promoteAgentTools(nil, original)

	// Caller mutates the slice the agent owns.
	original[0] = "tampered"

	got, _ := engine.GetDep[[]string](deps, depname.ToolAllowedNames)
	want := []string{"search", "fetch"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot drift: got %v want %v (helper must defensively copy ag.Tools)", got, want)
	}
}
