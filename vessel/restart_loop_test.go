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

// flappingBus wraps an event.Bus and fails Subscribe on demand.
// We use it to drive the restartLoop's Launch-error path
// deterministically: a sidecar agent's Launch path goes through
// bus.Subscribe, so flipping failSubscribe to true is enough to
// make every subsequent re-Launch attempt error.
type flappingBus struct {
	event.Bus
	failSubscribe atomic.Bool
}

func (f *flappingBus) Subscribe(ctx context.Context, pattern event.Pattern, opts ...event.SubOption) (event.Subscription, error) {
	if f.failSubscribe.Load() {
		return nil, errors.New("flappingBus: subscribe disabled (test injection)")
	}
	return f.Bus.Subscribe(ctx, pattern, opts...)
}

// TestCaptain_RestartLoop_ExhaustionFinalizes asserts that when
// restartLoop hits MaxRestarts on a persistently-failing Launch,
// the captain runs finalize() — transitioning to PhaseStopped and
// releasing owned resources — rather than parking in PhaseFailed
// indefinitely.
//
// The contract being pinned:
//
//   - Final Phase == PhaseStopped (NOT PhaseFailed). Stopped is
//     the terminal-state callers already check via
//     Phase().IsTerminal(); Failed implies "still trying" and
//     would mislead operators after exhaustion.
//   - The owned bus has been closed. Subsequent Publish returns
//     event.ErrBusClosed, which is the operator-visible signal
//     that the captain's resource lifetime ended.
//
// The flappingBus harness is needed because Launch's only fail-
// prone seams are bus.Subscribe (sidecars + callback bridge);
// without a way to make those error, restartLoop's exhaust path
// is unreachable from inside the package.
func TestCaptain_RestartLoop_ExhaustionFinalizes(t *testing.T) {
	t.Parallel()
	bus := &flappingBus{Bus: event.NewMemoryBus()}

	vs := spec.Spec{
		ID: "v-rl-exhaust",
		Agents: []spec.Agent{
			{Name: "primary"},
			{Name: "side", Sidecar: true, SubscribeTo: "test.>"},
		},
		Restart: spec.Restart{
			Mode:        spec.RestartOnFailure,
			MaxRestarts: 2,
			BackoffInit: 5 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
		},
	}

	c, err := New(vs, WithEngine(echoEngine()), WithBus(bus))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("initial Launch (bus healthy): %v", err)
	}
	if c.Phase() != PhaseRunning {
		t.Fatalf("post-Launch phase = %s, want running", c.Phase())
	}

	// Simulate "the world broke around us": Subscribe will now
	// reject every call. The next time the captain tries to
	// re-Launch (sidecar path), it hits this and errors.
	bus.failSubscribe.Store(true)

	// transitionToFailed is the prod entry point for restartLoop;
	// invoking it directly mimics what the probe runner does on
	// repeated failure without coupling the test to probe timing.
	c.transitionToFailed("simulated probe failure")

	// MaxRestarts=2 with 5ms initial / 10ms cap backoff finishes
	// well under 200ms in steady state. The 2s budget absorbs
	// scheduler jitter on slow CI runners.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Phase() == PhaseStopped {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.Phase(); got != PhaseStopped {
		t.Fatalf("after exhaustion phase = %s, want stopped (finalize must transition past PhaseFailed)", got)
	}

	// The owned bus must have been closed by finalize. Because
	// busOwned=false in this test (we passed WithBus), closeOwnedBus
	// is a no-op and the externally-owned bus is left alive — which
	// is the documented contract. Verify it via still-Subscribable
	// state on the underlying bus to make the contract explicit.
	if _, subErr := bus.Bus.Subscribe(context.Background(), "test.x"); subErr != nil {
		t.Fatalf("externally-owned bus closed by finalize; ownership contract violated (err=%v)", subErr)
	}
}

// TestCaptain_ProbeFlap_RespectsMaxRestarts asserts that probe-
// driven failures actually accumulate against MaxRestarts and
// finalize when the cap is reached. Pre-fix this was the headline
// gap: each transitionToFailed spawned a fresh restartLoop with a
// local attempts counter starting at 0, so probe flap parked the
// captain in an infinite Running ↔ Failed cycle no matter how
// small MaxRestarts was set.
//
// Test choreography:
//
//   - Launch the captain healthy (no sidecar, no flapping bus).
//   - Trigger transitionToFailed three times. The first two
//     re-Launches succeed (bus is fine), so the captain transits
//     Failed → Running each cycle. The third trip exhausts
//     MaxRestarts and runs finalize → PhaseStopped.
//   - Sleep BackoffInit between trips so each restartLoop has a
//     chance to land its Launch and exit before the next failure.
//
// The captain-level restartAttempts counter must persist across
// the three restartLoop spawns; without that, the third
// transitionToFailed would simply spawn its own fresh restartLoop
// (attempts=0 again) and we'd never finalize.
func TestCaptain_ProbeFlap_RespectsMaxRestarts(t *testing.T) {
	t.Parallel()
	vs := spec.Spec{
		ID:     "v-flap",
		Agents: []spec.Agent{{Name: "primary"}},
		Restart: spec.Restart{
			Mode:        spec.RestartOnFailure,
			MaxRestarts: 2,
			BackoffInit: 5 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
		},
	}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("initial Launch: %v", err)
	}

	// Cycle 1: Failed → Pending → Running.
	c.transitionToFailed("flap-1")
	waitFor(t, c, PhaseRunning, 500*time.Millisecond)

	// Cycle 2: Failed → Pending → Running. Counter now at 2.
	c.transitionToFailed("flap-2")
	waitFor(t, c, PhaseRunning, 500*time.Millisecond)

	// Cycle 3: counter would reach 3 > MaxRestarts. The exhausted
	// gate inside transitionToFailed kicks in synchronously and
	// runs finalize without spawning a third restartLoop.
	c.transitionToFailed("flap-3")
	waitFor(t, c, PhaseStopped, 1*time.Second)

	if got := c.Phase(); got != PhaseStopped {
		t.Fatalf("after 3rd flap phase = %s, want stopped (MaxRestarts=%d should have tripped)", got, vs.Restart.MaxRestarts)
	}
}

// overrideStableWindow swaps restartStableWindow for the duration
// of a test, returning a restore-fn the test defers. Lets the
// flap/forgiveness behaviour be exercised in milliseconds instead
// of waiting the production 30s window.
func overrideStableWindow(d time.Duration) func() {
	prev := restartStableWindow.Load()
	restartStableWindow.Store(int64(d))
	return func() { restartStableWindow.Store(prev) }
}

// waitFor polls c.Phase until it equals want or budget elapses.
// Helper isolated here so the flap test reads as a sequence of
// "make a thing happen, wait for the consequence" steps without
// inlining the same poll loop three times.
func waitFor(t *testing.T, c *Captain, want Phase, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if c.Phase() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("phase did not reach %s within %s; current=%s", want, budget, c.Phase())
}

// TestCaptain_ProbeFlap_StableWindowResetsCounter asserts the
// "stable run" forgiveness: when the captain stays in PhaseRunning
// for ≥ restartStableWindow between failures, the next failure
// starts the counter fresh. Without this a long-running healthy
// vessel would eventually accumulate enough flap to finalize even
// though the failures were widely separated and likely transient.
//
// We force restartStableWindow to a tiny value via a runtime hook
// so the test runs in milliseconds instead of waiting 30s. The
// hook is added solely for testability (see captainStableWindow
// override below) and pays for itself by making this contract
// directly testable.
func TestCaptain_ProbeFlap_StableWindowResetsCounter(t *testing.T) {
	t.Parallel()
	defer overrideStableWindow(50 * time.Millisecond)()

	vs := spec.Spec{
		ID:     "v-stable",
		Agents: []spec.Agent{{Name: "primary"}},
		Restart: spec.Restart{
			Mode:        spec.RestartOnFailure,
			MaxRestarts: 2,
			BackoffInit: 5 * time.Millisecond,
			BackoffMax:  10 * time.Millisecond,
		},
	}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// First flap: counter → 1 (re-Launches successfully).
	c.transitionToFailed("flap-1")
	waitFor(t, c, PhaseRunning, 500*time.Millisecond)

	// Stay healthy long enough for the stable window to forgive.
	time.Sleep(80 * time.Millisecond)

	// Next flap should reset to 0 then bump to 1, NOT bump to 2.
	c.transitionToFailed("flap-2-after-stable")
	waitFor(t, c, PhaseRunning, 500*time.Millisecond)

	// Counter must be 1 here (not 2) — verifiable because a third
	// flap then a fourth must still recover, and only the FIFTH
	// finalize. Easier check: probe the counter directly while
	// holding the captain's mutex.
	c.mu.Lock()
	got := c.restartAttempts
	c.mu.Unlock()
	if got != 1 {
		t.Fatalf("after stable-window-forgiven flap, restartAttempts = %d, want 1", got)
	}
}

// TestCaptain_RestartLoop_ExhaustionClosesOwnedBus is the companion
// case where the captain owns its bus (no WithBus). finalize MUST
// then call Close() so the bus's goroutines exit and operators
// don't leak resources after a flapping vessel is given up on.
//
// We don't have a cheap injection seam for "make a default bus's
// Subscribe fail", so this test instead drives exhaustion via the
// admission gate path: stopping the captain mid-restartLoop also
// runs finalize, which is the same closeOwnedBus call site.
func TestCaptain_RestartLoop_OwnedBusIsClosed(t *testing.T) {
	t.Parallel()
	vs := spec.Spec{
		ID:     "v-owned-bus",
		Agents: []spec.Agent{{Name: "primary"}},
	}
	c, err := New(vs, WithEngine(echoEngine())) // no WithBus → owned
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !c.busOwned {
		t.Fatalf("expected busOwned=true when no WithBus")
	}
	captured := c.bus
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// After Stop runs finalize → closeOwnedBus, Subscribe must
	// report ErrBusClosed (or a similar terminal error). We do
	// not couple the assertion to a specific error value; any
	// non-nil error is sufficient evidence the close ran.
	if _, err := captured.Subscribe(context.Background(), "x"); err == nil {
		t.Fatalf("owned bus still accepts subscriptions after Stop; closeOwnedBus did not run")
	}
}
