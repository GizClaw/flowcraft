package pool

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDoAbsorbsRetryableGuards pins the contract every caller used to
// spell out by hand: a retryable lifecycle guard is absorbed by asking
// for the key's member again and running the call once more, and the
// caller never sees it.
func TestDoAbsorbsRetryableGuards(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/guarded"}
	pooled := h.pooled(k, 0)

	var calls int
	err := h.pool.Do(context.Background(), k, nil,
		func(m *fakeMember) error {
			calls++
			if m != pooled {
				t.Fatalf("fn got member %p, want %p", m, pooled)
			}
			if calls == 1 {
				return errGuard
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Do = %v, want nil after absorbing the guard", err)
	}
	if calls != 2 {
		t.Fatalf("fn calls = %d, want 2 (one refused, one accepted)", calls)
	}
}

// TestDoReportsNonRetryableErrorsAsTheyAre pins the other half: an
// error that is not a lifecycle guard is the caller's own failure, and
// repeating it would only delay the report. The same holds for a pool
// refusal — a key that names nothing must not be retried into the
// window.
func TestDoReportsNonRetryableErrorsAsTheyAre(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/refused"}
	h.pooled(k, 0)

	refused := errors.New("conversation is busy")
	var calls int
	err := h.pool.Do(context.Background(), k, nil,
		func(*fakeMember) error {
			calls++
			return refused
		})
	if !errors.Is(err, refused) {
		t.Fatalf("Do = %v, want %v", err, refused)
	}
	if calls != 1 {
		t.Fatalf("fn calls = %d, want 1: a non-retryable error is reported", calls)
	}

	err = h.pool.Do(context.Background(), Key{}, nil,
		func(*fakeMember) error {
			t.Fatal("fn ran for a key that names nothing")
			return nil
		})
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("Do = %v, want ErrNoKey", err)
	}
	if got := h.buildCount(); got != 0 {
		t.Fatalf("assemblies = %d, want 0: a refusal is not retried", got)
	}
}

// TestDoStopsWhenTheCallerPremiseExpired pins the stop guard and where
// it is consulted. A caller whose premise has already expired still
// gets its first attempt — the call is resolved against the key it
// named, not against whatever the application happens to show — and
// gives up on the retry instead of holding the call open for a key
// nobody is looking at.
func TestDoStopsWhenTheCallerPremiseExpired(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/moved"}
	h.pooled(k, 0)

	var calls int
	err := h.pool.Do(context.Background(), k,
		func() bool { return true },
		func(*fakeMember) error {
			calls++
			return errGuard
		})
	if !errors.Is(err, errGuard) {
		t.Fatalf("Do = %v, want the last lifecycle guard", err)
	}
	if calls != 1 {
		t.Fatalf("fn calls = %d, want 1: one attempt, then no retry", calls)
	}
}

// TestDoSurfacesTheCallersOwnCancellation pins the difference between
// the window running out and the call dying: only the former is
// absorbed. A canceled call reports the cancellation instead of a stale
// lifecycle guard nobody can act on.
func TestDoSurfacesTheCallersOwnCancellation(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/canceled"}
	release := make(chan struct{})
	defer close(release)
	h.mu.Lock()
	h.openGate = release
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := h.pool.Do(ctx, k, nil, func(*fakeMember) error {
		t.Fatal("fn ran without a member")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v, want the caller's own cancellation", err)
	}
}

// TestDoBoundsTheWaitForAReplacement pins the window: a key whose old
// member never finishes draining does not hold the call open for the
// length of somebody else's work. The caller gets the guard it hit, not
// the wait's own deadline. The case shortens the window so it asserts
// on the bound without paying the production one.
func TestDoBoundsTheWaitForAReplacement(t *testing.T) {
	window := 200 * time.Millisecond
	h := newHarness(t, func(spec *Spec[fakeMember]) {
		spec.RetryWindow = window
	})
	k := Key{"workspace", "/ws/bounded"}
	blocked := make(chan struct{})
	defer close(blocked)
	h.mu.Lock()
	h.openGate = blocked
	h.mu.Unlock()

	start := time.Now()
	err := h.pool.Do(context.Background(), k, nil, func(*fakeMember) error {
		t.Fatal("fn ran without a member")
		return nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Do = %v, want the not-ready guard it never got past", err)
	}
	if elapsed < window {
		t.Fatalf("Do gave up after %v, want the window %v", elapsed, window)
	}
	if elapsed > window+5*time.Second {
		t.Fatalf("Do waited %v, want the wait bounded by %v", elapsed, window)
	}
	// One window, one attempt: the budget bounds the whole call, not
	// each attempt. A per-attempt window would have started three
	// assemblies here.
	if got := h.buildCount(); got != 1 {
		t.Fatalf("assembly attempts = %d, want 1 inside one window", got)
	}
}

// TestDoUsesAMemberResolvedAsTheWindowClosed pins where the expiry check
// sits. A resolution can land after the attempt's deadline has already
// passed — the pool finished the teardown just as the window ran out —
// and reporting "not ready" then drops a usable member on the floor: the
// caller refuses a request the key can serve, and only the next call
// gets the member that was there all along.
func TestDoUsesAMemberResolvedAsTheWindowClosed(t *testing.T) {
	window := 100 * time.Millisecond
	h := newHarness(t, func(spec *Spec[fakeMember]) {
		spec.RetryWindow = window
	})
	k := Key{"workspace", "/ws/late"}
	gate := make(chan struct{})
	h.mu.Lock()
	h.openGate = gate
	h.openIgnoresCtx = true
	h.mu.Unlock()
	go func() {
		time.Sleep(3 * window)
		close(gate)
	}()

	var calls int
	err := h.pool.Do(context.Background(), k, nil, func(*fakeMember) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Do = %v, want nil: the member was resolved and usable", err)
	}
	if calls != 1 {
		t.Fatalf("fn calls = %d, want 1", calls)
	}
}

// TestDoRetriesUntilTheAttemptBudgetRunsOut pins the attempt cap on top
// of the window: a guard that never clears is retried
// DefaultRetryAttempts times and no more, and what the caller saw last
// is what comes back. The first attempt always runs, whatever the cap.
func TestDoRetriesUntilTheAttemptBudgetRunsOut(t *testing.T) {
	t.Run("default budget", func(t *testing.T) {
		h := newHarness(t)
		k := Key{"workspace", "/ws/exhausted"}
		h.pooled(k, 0)

		var calls int
		err := h.pool.Do(context.Background(), k, nil,
			func(*fakeMember) error {
				calls++
				return errGuard
			})
		if !errors.Is(err, errGuard) {
			t.Fatalf("Do = %v, want the guard it hit last", err)
		}
		if calls != DefaultRetryAttempts {
			t.Fatalf("fn calls = %d, want %d", calls, DefaultRetryAttempts)
		}
	})

	t.Run("one attempt", func(t *testing.T) {
		h := newHarness(t, func(spec *Spec[fakeMember]) {
			spec.RetryAttempts = 1
		})
		k := Key{"workspace", "/ws/one-attempt"}
		h.pooled(k, 0)

		var calls int
		err := h.pool.Do(context.Background(), k, nil,
			func(*fakeMember) error {
				calls++
				return errGuard
			})
		if !errors.Is(err, errGuard) {
			t.Fatalf("Do = %v, want the guard it hit", err)
		}
		if calls != 1 {
			t.Fatalf("fn calls = %d, want 1", calls)
		}
	})
}

// TestDoPausesBetweenAttempts pins the backoff: a guard that fails
// instantly is still spaced out, so the retry waits for the premise to
// change instead of spinning through its attempt budget. The bound is
// the window, which the pause never runs past.
func TestDoPausesBetweenAttempts(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/backoff"}
	h.pooled(k, 0)

	var attempts []time.Time
	err := h.pool.Do(context.Background(), k, nil,
		func(*fakeMember) error {
			attempts = append(attempts, time.Now())
			return errGuard
		})
	if !errors.Is(err, errGuard) {
		t.Fatalf("Do = %v, want the guard it hit last", err)
	}
	if len(attempts) != DefaultRetryAttempts {
		t.Fatalf("attempts = %d, want %d", len(attempts), DefaultRetryAttempts)
	}
	if gap := attempts[1].Sub(attempts[0]); gap < retryBackoff {
		t.Fatalf("attempts ran %v apart, want at least %v", gap, retryBackoff)
	}
}
