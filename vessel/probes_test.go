package vessel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// flakyProbe reports unhealthy until counter reaches threshold,
// after which it reports healthy. Used to drive the failure-streak
// machinery deterministically.
type flakyProbe struct {
	counter atomic.Int32
	ok      int32
}

func (p *flakyProbe) Name() string { return "flaky" }
func (p *flakyProbe) Check(_ context.Context) (spec.ProbeResult, error) {
	n := p.counter.Add(1)
	if n >= p.ok {
		return spec.ProbeResult{Healthy: true}, nil
	}
	return spec.ProbeResult{Healthy: false, Reason: "still warming up"}, nil
}

// TestProbe_FailsToFailedPhase asserts that a probe failing more
// than the configured FailureThreshold flips the Captain into
// PhaseFailed and emits a SubjectProbeFailed envelope.
func TestProbe_FailsToFailedPhase(t *testing.T) {
	t.Parallel()
	probe := &flakyProbe{ok: 100} // never recovers
	vs := spec.Spec{
		Agents: []spec.Agent{{Name: "p"}},
		Probes: &spec.Probes{
			Liveness:         []spec.Probe{probe},
			Interval:         15 * time.Millisecond,
			Timeout:          50 * time.Millisecond,
			FailureThreshold: 2,
		},
	}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())

	sub, err := c.Bus().Subscribe(context.Background(), event.Pattern(SubjectProbeFailed))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// First failure envelope.
	select {
	case env, ok := <-sub.C():
		if !ok {
			t.Fatal("subscription closed early")
		}
		if env.Subject != SubjectProbeFailed {
			t.Fatalf("subject = %s, want %s", env.Subject, SubjectProbeFailed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first probe failure")
	}

	// Wait for the Captain to flip to Failed (>= threshold).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Phase() == PhaseFailed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("phase never reached Failed; current = %s", c.Phase())
}

// TestProbe_HealthyResetsStreak validates the streak counter is
// cleared on a healthy round, so transient flakes below the
// threshold do NOT cause a flip.
func TestProbe_HealthyResetsStreak(t *testing.T) {
	t.Parallel()
	probe := &flakyProbe{ok: 2} // first call unhealthy, second healthy onwards
	vs := spec.Spec{
		Agents: []spec.Agent{{Name: "p"}},
		Probes: &spec.Probes{
			Liveness:         []spec.Probe{probe},
			Interval:         10 * time.Millisecond,
			Timeout:          50 * time.Millisecond,
			FailureThreshold: 3,
		},
	}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Give the loop several ticks to confirm we never trip into
	// Failed (one bad → reset → many good ticks).
	time.Sleep(150 * time.Millisecond)
	if c.Phase() == PhaseFailed {
		t.Fatalf("Phase flipped to Failed despite recovery")
	}
}

// TestProbe_RestartOnFailure confirms the captain re-runs Launch
// after entering Failed when Restart.Mode == RestartOnFailure.
func TestProbe_RestartOnFailure(t *testing.T) {
	t.Parallel()
	// flakyProbe with ok=4: rounds 1..3 unhealthy → Failed; the
	// captain restarts → on round 4 onwards healthy.
	probe := &flakyProbe{ok: 4}
	vs := spec.Spec{
		Agents: []spec.Agent{{Name: "p"}},
		Probes: &spec.Probes{
			Liveness:         []spec.Probe{probe},
			Interval:         10 * time.Millisecond,
			Timeout:          50 * time.Millisecond,
			FailureThreshold: 3,
		},
		Restart: spec.Restart{
			Mode:        spec.RestartOnFailure,
			BackoffInit: 5 * time.Millisecond,
			BackoffMax:  20 * time.Millisecond,
			MaxRestarts: 3,
		},
	}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Confirm a restart happened: the captain should bump
	// restartAttempts (persists across generations) AND end up
	// healthy in PhaseRunning. We can't reliably catch the
	// transient PhaseFailed window via polling under -race; the
	// restart counter is the persistent artefact of the cycle.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		attempts := c.restartAttempts
		c.mu.Unlock()
		if attempts >= 1 && c.Phase() == PhaseRunning {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	attempts := c.restartAttempts
	c.mu.Unlock()
	t.Fatalf("never observed restart cycle (restartAttempts=%d, phase=%s)", attempts, c.Phase())
}

// TestProbe_PanicCaught ensures a panicking probe surfaces as a
// failure rather than crashing the loop.
func TestProbe_PanicCaught(t *testing.T) {
	t.Parallel()
	panicProbe := spec.ProbeFunc{
		Label: "boom",
		Fn: func(_ context.Context) (spec.ProbeResult, error) {
			panic(errors.New("kaboom"))
		},
	}
	vs := spec.Spec{
		Agents: []spec.Agent{{Name: "p"}},
		Probes: &spec.Probes{
			Liveness:         []spec.Probe{panicProbe},
			Interval:         10 * time.Millisecond,
			Timeout:          50 * time.Millisecond,
			FailureThreshold: 1,
		},
	}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Phase() == PhaseFailed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Failed not reached; panic must have crashed the loop. phase=%s", c.Phase())
}
