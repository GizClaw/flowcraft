package provider

import (
	"testing"
	"time"
)

// TestCircuit_HalfOpenConsecutiveFailuresStayBounded is a white-box regression
// test for the half-open bounded-count fix (issue #4b).
//
// When a circuit's open window elapses, Allow transitions it to half-open and
// decays consecutiveFailures to 0, and a probe failure re-opens the circuit
// from that clean count. As a result the internal consecutiveFailures counter
// must stay bounded no matter how many open -> probe-fail cycles occur.
//
// The existing black-box test (TestCircuit_HalfOpenProbeFailureReopensFromClean
// Count) only checks a single re-open; it does not catch a counter that grows
// without bound across many cycles. This test drives many cycles and asserts
// the counter never exceeds the break threshold — before the fix it would grow
// roughly linearly with the number of cycles.
func TestCircuit_HalfOpenConsecutiveFailuresStayBounded(t *testing.T) {
	c := NewCircuit(1)
	policy := FallbackPolicy{CircuitBreakAfter: 2, CircuitOpen: 50 * time.Millisecond}
	base := time.Now()

	// Open the circuit initially: two failures reach the break threshold.
	c.OnFailure(0, base, policy, true)
	if !c.OnFailure(0, base, policy, true) {
		t.Fatal("circuit should open after reaching CircuitBreakAfter")
	}

	maxSeen := 0
	record := func() {
		// Same-package white-box access to the internal counter.
		c.mu.Lock()
		got := c.states[0].consecutiveFailures
		c.mu.Unlock()
		if got > maxSeen {
			maxSeen = got
		}
	}
	record()

	now := base
	const cycles = 200
	for cycle := 0; cycle < cycles; cycle++ {
		// Let the open window elapse so Allow yields a half-open probe.
		now = now.Add(60 * time.Millisecond)
		if !c.Allow(0, now) {
			t.Fatalf("cycle %d: expected a half-open probe to be allowed", cycle)
		}
		// The probe fails, which must re-open the circuit from a clean count.
		if !c.OnFailure(0, now, policy, true) {
			t.Fatalf("cycle %d: half-open probe failure should re-open the circuit", cycle)
		}
		record()
	}

	// Bounded property: the counter must not grow with the number of cycles.
	// Before the fix it would be ~= 2 + cycles.
	if maxSeen > policy.CircuitBreakAfter {
		t.Fatalf("consecutiveFailures grew unbounded across %d cycles: max=%d, want <= %d",
			cycles, maxSeen, policy.CircuitBreakAfter)
	}
}

// TestCircuit_HalfOpenAllowsSingleProbe verifies that once the open window
// elapses, Allow hands out exactly one half-open probe and holds every other
// caller until the probe is resolved via OnSuccess/OnFailure. Before the fix,
// the first probe zeroed openUntil and any concurrent/subsequent Allow saw a
// zero window and returned true, letting a post-cooldown burst hit a possibly
// still-unhealthy provider.
func TestCircuit_HalfOpenAllowsSingleProbe(t *testing.T) {
	policy := FallbackPolicy{CircuitBreakAfter: 1, CircuitOpen: 50 * time.Millisecond}
	base := time.Now()

	open := func() *Circuit {
		c := NewCircuit(1)
		if !c.OnFailure(0, base, policy, true) {
			t.Fatal("circuit should open after reaching CircuitBreakAfter")
		}
		return c
	}
	afterCooldown := base.Add(60 * time.Millisecond)

	t.Run("holds other callers until probe resolves", func(t *testing.T) {
		c := open()
		if !c.Allow(0, afterCooldown) {
			t.Fatal("first caller after cooldown should get the half-open probe")
		}
		// Probe is in flight; all other callers must be held.
		for i := 0; i < 5; i++ {
			if c.Allow(0, afterCooldown) {
				t.Fatalf("call %d: expected probe to be held while one is in flight", i)
			}
		}
	})

	t.Run("probe success reopens the gate", func(t *testing.T) {
		c := open()
		if !c.Allow(0, afterCooldown) {
			t.Fatal("expected half-open probe")
		}
		c.OnSuccess(0)
		if !c.Allow(0, afterCooldown) {
			t.Fatal("after a successful probe the circuit should be closed and allow traffic")
		}
	})

	t.Run("probe failure re-opens and blocks", func(t *testing.T) {
		c := open()
		if !c.Allow(0, afterCooldown) {
			t.Fatal("expected half-open probe")
		}
		if !c.OnFailure(0, afterCooldown, policy, true) {
			t.Fatal("failed probe should re-open the circuit")
		}
		if c.Allow(0, afterCooldown) {
			t.Fatal("re-opened circuit should block until the new cooldown elapses")
		}
	})
}
