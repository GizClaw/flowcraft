package host

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool/platform"
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
			_, denied := policy.AsDenial(err)
			if !tc.want && !denied && !errdefs.IsValidation(err) {
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

// asyncHost installs a parent that may delegate to a child asynchronously, over one process.
func asyncHost(t *testing.T) (*Agents, *sessionProgram, *sessionProgram) {
	t.Helper()
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)

	parentGrant := permissiveGrant("local")
	parentGrant.Delegation = &policy.Delegation{
		Targets: []string{"child"},
		Modes:   []policy.DelegationMode{policy.DelegationAsync},
	}
	parent, parentProgram := sessionDeployment(t, "parent", parentGrant, log, backend)
	child, childProgram := sessionDeployment(t, "child", permissiveGrant("local"), log, backend)
	if err := agents.Add(parent, child); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "p1", "parent", parentGrant); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	return agents, parentProgram, childProgram
}

// TestAnAsyncDelegationActuallyRuns is the async edge's whole promise: the child runs after the
// asking turn returns. The child's session has to outlive the call that opened it — a close
// registered by the caller erased it before the goroutine could start, and the caller was told
// "submitted" about a run that never happened.
func TestAnAsyncDelegationActuallyRuns(t *testing.T) {
	agents, _, childProgram := asyncHost(t)
	parent, _ := agents.Session("p1")

	delegator := delegatorOf(agents, parent)
	outcome, err := delegator.Delegate(t.Context(), platform.DelegationRequest{
		Target: "child", Mode: "async", Input: "please review",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if outcome.Status != string(agent.StateSubmitted) {
		t.Fatalf("status = %q, want submitted", outcome.Status)
	}

	// The goroutine closes the child when the run ends; that is also the signal that it ran.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, open := agents.Session(outcome.Session); !open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child session %q was never closed; the run did not happen", outcome.Session)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := childProgram.opens.Load(); got != 1 {
		t.Fatalf("the child program opened %d drivers, want 1", got)
	}
}

// TestStatusWithoutAStoreIsRefused keeps a model-supplied tool call from panicking: the store is
// optional, and a host that has none cannot answer where a child stands.
func TestStatusWithoutAStoreIsRefused(t *testing.T) {
	agents, _, _ := asyncHost(t)
	parent, _ := agents.Session("p1")
	delegator := delegatorOf(agents, parent)
	if _, err := delegator.Status(t.Context(), "p1/child-1"); err == nil {
		t.Fatal("Status answered without a store")
	} else if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Status = %v, want NotAvailable", err)
	}
}

// TestAChildInheritsItsParentsAuthority is the default rule: a delegation cannot manufacture
// authority, so with no resolver the child runs under exactly what the parent held.
func TestAChildInheritsItsParentsAuthority(t *testing.T) {
	agents, _, _ := asyncHost(t)
	parent, _ := agents.Session("p1")
	d := delegatorOf(agents, parent)

	inherited, err := d.childAuthority(t.Context(), "child")
	if err != nil {
		t.Fatalf("childAuthority: %v", err)
	}
	if inherited.Digest() != parent.grant.Digest() {
		t.Fatalf("child authority = %s, want the parent's %s", inherited.Digest(), parent.grant.Digest())
	}
}

// TestTheApplicationDecidesAChildsAuthority is the override: an application that needs per-target
// authority answers, and the host takes the answer as given.
func TestTheApplicationDecidesAChildsAuthority(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	narrow := permissiveGrant("local")
	narrow.Tools = policy.ToolSet{Allow: []string{"read_file"}}

	var askedTarget string
	var askedAuthority policy.Grant
	agents := hostWith(t, backend, WithDelegationGrant(func(_ context.Context, parent, target string, authority policy.Grant) (policy.Grant, error) {
		askedTarget = target
		askedAuthority = authority
		if parent != "parent" {
			t.Errorf("parent = %q, want the asking agent", parent)
		}
		return narrow, nil
	}))
	parentGrant := permissiveGrant("local")
	parentGrant.Delegation = &policy.Delegation{
		Targets: []string{"child"},
		Modes:   []policy.DelegationMode{policy.DelegationAsync},
	}
	parent, _ := sessionDeployment(t, "parent", parentGrant, log, backend)
	child, _ := sessionDeployment(t, "child", permissiveGrant("local"), log, backend)
	if err := agents.Add(parent, child); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "p1", "parent", parentGrant); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	session, _ := agents.Session("p1")

	got, err := delegatorOf(agents, session).childAuthority(t.Context(), "child")
	if err != nil {
		t.Fatalf("childAuthority: %v", err)
	}
	if got.Digest() != narrow.Digest() {
		t.Fatalf("authority = %s, want the resolver's %s", got.Digest(), narrow.Digest())
	}
	if askedTarget != "child" || askedAuthority.Digest() != parentGrant.Digest() {
		t.Fatalf("resolver asked about %q with %s, want the target and the parent's authority",
			askedTarget, askedAuthority.Digest())
	}
}

// delegatorOf builds the delegator a session gets at open, from the values it holds.
func delegatorOf(agents *Agents, parent *Session) *delegator {
	d := &delegator{
		agents: agents,
		parent: parent.id,
		agent:  parent.agent,
		grant:  parent.grant,
		chain:  parent.chain,
	}
	if limit := parent.grant.Delegation.MaxConcurrent; limit > 0 {
		d.gate = make(chan struct{}, limit)
	}
	return d
}
