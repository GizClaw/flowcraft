package host

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/policy"
)

// A delegation is admitted by three facts and refused by each of them: which agent, which mode, how
// deep. They are checked together because a caller that got one of them wrong needs to hear which.
func TestAdmitDelegationChecksTargetModeAndDepth(t *testing.T) {
	allowed := policy.Delegation{
		Targets:  []string{"seat_1", "seat_2"},
		Modes:    []policy.DelegationMode{policy.DelegationSync},
		MaxDepth: 2,
	}
	for name, tc := range map[string]struct {
		policy *policy.Delegation
		target string
		mode   string
		depth  int
		want   bool
	}{
		"admitted":               {&allowed, "seat_1", "sync", 0, true},
		"no policy at all":       {nil, "seat_1", "sync", 0, false},
		"empty policy":           {&policy.Delegation{}, "seat_1", "sync", 0, false},
		"target not listed":      {&allowed, "seat_9", "sync", 0, false},
		"mode not listed":        {&allowed, "seat_1", "async", 0, false},
		"too deep":               {&allowed, "seat_1", "sync", 2, false},
		"empty target":           {&allowed, "", "sync", 0, false},
		"depth zero max is open": {&policy.Delegation{Targets: []string{"a"}}, "a", "sync", 42, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := admitDelegation(tc.policy, tc.target, tc.mode, identity.Chain{ID: "c", Depth: tc.depth})
			if tc.want && err != nil {
				t.Fatalf("admitDelegation = %v, want admitted", err)
			}
			if !tc.want && !errdefs.IsForbidden(err) && !errdefs.IsValidation(err) {
				t.Fatalf("admitDelegation = %v, want a refusal", err)
			}
		})
	}
}
