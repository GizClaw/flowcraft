package host

import (
	"context"
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

// The three limits the policy gained: an input cap that is a permission, a timeout that stops a child
// from taking its parent's turn with it, and a fan-out gate. The first and the third are checked
// without running anything, which is what makes them testable before a child deployment exists.
func TestDelegationLimitsAreThePolicys(t *testing.T) {
	limited := policy.Delegation{
		Targets:       []string{"seat_1"},
		Modes:         []policy.DelegationMode{policy.DelegationSync},
		MaxInputBytes: 8,
	}
	if limit := limited.InputLimit(); limit != 8 {
		t.Fatalf("InputLimit = %d, want the policy's 8", limit)
	}
	if limit := (&policy.Delegation{}).InputLimit(); limit != policy.DefaultMaxInputBytes {
		t.Fatalf("InputLimit = %d, want the default", limit)
	}

	gate := &delegator{gate: make(chan struct{}, 1)}
	release, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	blocked, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.acquire(blocked); !errdefs.IsInterrupted(err) && err == nil {
		t.Fatal("a full gate admitted a second edge")
	}
	release()
	if _, err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}
