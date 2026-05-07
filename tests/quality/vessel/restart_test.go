package vesselquality

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"

	"github.com/GizClaw/flowcraft/tests/quality/vessel/fakellm"
)

// TestRestartOnFailure asserts the Captain re-launches itself
// after a liveness probe trips PhaseFailed. The probe reports
// Unhealthy for the first N rounds, then becomes healthy on the
// restart attempt — we expect the Captain to land back in
// PhaseRunning within MaxRestarts.
func TestRestartOnFailure(t *testing.T) {
	t.Parallel()

	var checks int32
	probe := spec.ProbeFunc{
		Label: "flaky",
		Fn: func(_ context.Context) (spec.ProbeResult, error) {
			n := atomic.AddInt32(&checks, 1)
			// Fail the first 3 checks, then go healthy. The
			// FailureThreshold of 1 means a single failure flips
			// to PhaseFailed; restart waits BackoffInit then
			// re-Launches, which fires probe round #2 — also
			// fails, …, until #4 returns Healthy.
			if n < 4 {
				return spec.ProbeResult{Healthy: false, Reason: "warming up"}, nil
			}
			return spec.ProbeResult{Healthy: true}, nil
		},
	}

	vs := spec.Spec{
		ID:     "v-restart",
		Agents: []spec.Agent{{Name: "noop"}},
		Probes: &spec.Probes{
			Liveness:         []spec.Probe{probe},
			Interval:         50 * time.Millisecond,
			Timeout:          time.Second,
			FailureThreshold: 1,
		},
		Restart: spec.Restart{
			Mode:        spec.RestartOnFailure,
			BackoffInit: 50 * time.Millisecond,
			BackoffMax:  200 * time.Millisecond,
			MaxRestarts: 8,
		},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{
			"noop": fakellm.New(nil, fakellm.WithRepeatLast()),
		}, 1)),
	)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Phase() == vessel.PhaseRunning && atomic.LoadInt32(&checks) >= 4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("captain never recovered: phase=%s checks=%d", c.Phase(), atomic.LoadInt32(&checks))
}

// TestMaxRestartsExhausted asserts that when the probe NEVER
// recovers, MaxRestarts caps the attempts and the Captain
// finalises into PhaseStopped (no infinite loop).
//
// The earlier version of this test asserted the captain stayed
// in PhaseFailed forever — that was a regression-locked-in of the
// bug where probe-driven flap reset the per-restartLoop attempt
// counter on each spawn. With the captain-level shared counter
// (see vessel/captain.go restartAttempts), the cap actually trips
// and finalize transitions to Stopped. We retain the "no further
// transitions" check by re-asserting Stopped after a settle
// window.
func TestMaxRestartsExhausted(t *testing.T) {
	t.Parallel()

	probe := spec.ProbeFunc{
		Label: "always-bad",
		Fn: func(_ context.Context) (spec.ProbeResult, error) {
			return spec.ProbeResult{Healthy: false, Reason: "broken"}, nil
		},
	}
	vs := spec.Spec{
		ID:     "v-restart-exhaust",
		Agents: []spec.Agent{{Name: "noop"}},
		Probes: &spec.Probes{
			Liveness:         []spec.Probe{probe},
			Interval:         30 * time.Millisecond,
			Timeout:          time.Second,
			FailureThreshold: 1,
		},
		Restart: spec.Restart{
			Mode:        spec.RestartOnFailure,
			BackoffInit: 30 * time.Millisecond,
			BackoffMax:  60 * time.Millisecond,
			MaxRestarts: 3,
		},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{
			"noop": fakellm.New(nil, fakellm.WithRepeatLast()),
		}, 1)),
	)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Phase() == vessel.PhaseStopped {
			// Settle window: confirm finalize is sticky and no
			// stray restart loop re-transitions out of Stopped.
			time.Sleep(200 * time.Millisecond)
			if c.Phase() != vessel.PhaseStopped {
				t.Fatalf("phase escaped Stopped after exhaust: %s", c.Phase())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("captain never reached PhaseStopped; current=%s", c.Phase())
}
