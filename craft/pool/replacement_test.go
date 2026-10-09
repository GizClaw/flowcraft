package pool

import (
	"context"
	"errors"
	"testing"
)

// TestRepeatedInvalidationsArmOneReplacement pins the once-per-key
// contract of the deferred rebuild: a storm of invalidations inside one
// drain — a settings save, a plugin write and a workspace switch all
// landing on one key — asks for one replacement, assembled after the
// drain, and announced once.
func TestRepeatedInvalidationsArmOneReplacement(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/armed-once"}
	old := h.pooled(k, 1)
	release := make(chan struct{})
	h.mu.Lock()
	h.openGate = release
	h.mu.Unlock()

	ctx := WithReason(context.Background(), "settings_save")
	h.pool.Invalidate(ctx, k)
	if !h.pool.ScheduleReplacement(ctx, k) {
		t.Fatal("the first arm was refused")
	}
	if h.pool.ScheduleReplacement(ctx, k) {
		t.Fatal("the second arm went through; one drain is one replacement")
	}
	if !h.pool.ReplacementArmed(k) {
		t.Fatal("the key does not report an armed replacement")
	}
	// A second invalidation inside the same drain changes nothing.
	h.pool.Invalidate(ctx, k)
	if h.pool.ScheduleReplacement(ctx, k) {
		t.Fatal("a third arm went through")
	}

	// Nothing is built while the old generation still serves work.
	if got := h.buildCount(); got != 0 {
		t.Fatalf("assemblies = %d, want 0 under a live unit of work", got)
	}
	h.assertNothingClosed()

	// The last unit ends: the old member retires, and the one armed
	// watcher assembles its replacement.
	h.finish(old)
	close(release)
	h.eventually("the replacement to land", func() bool {
		return !h.pool.ReplacementArmed(k)
	})
	if got := h.buildCount(); got != 1 {
		t.Fatalf("assemblies = %d, want exactly 1 replacement", got)
	}
	h.mu.Lock()
	replaced := append([]Key(nil), h.replaced...)
	h.mu.Unlock()
	if len(replaced) != 1 || replaced[0] != k {
		t.Fatalf("Replaced ran for %v, want once for %s", replaced, k)
	}
	fresh := h.pool.Current(k)
	if fresh == nil || fresh == old {
		t.Fatalf("Current = %p, want a fresh member", fresh)
	}
	if h.pool.Stale(k) {
		t.Fatal("the replaced key is still reported stale")
	}
}

// TestScheduleReplacementAssemblesWhenTheDrainIsAlreadyGone pins the
// race the watcher has to survive: a key's retired member can finish
// teardown between the moment a reload armed the replacement and the
// moment the watcher looks. "No member for this key" must not be read
// as "a live member serves it" — a key left unserved stays unserved
// until the next call otherwise — so the watcher assembles the
// replacement the arm asked for, and that replacement serves again.
func TestScheduleReplacementAssemblesWhenTheDrainIsAlreadyGone(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/drain-gone"}
	first := h.pooled(k, 0)

	h.pool.Invalidate(context.Background(), k)
	h.settleRetirement(k, first)
	if h.pool.Current(k) != nil {
		t.Fatal("the key is still served after the teardown settled")
	}

	if !h.pool.ScheduleReplacement(context.Background(), k) {
		t.Fatal("the replacement was not armed")
	}
	h.eventually("the replacement to land", func() bool {
		return !h.pool.ReplacementArmed(k)
	})
	h.mu.Lock()
	replaced := append([]Key(nil), h.replaced...)
	h.mu.Unlock()
	if len(replaced) != 1 || replaced[0] != k {
		t.Fatalf("Replaced ran for %v, want once for %s", replaced, k)
	}
	replacement := h.pool.Current(k)
	if replacement == nil || replacement == first {
		t.Fatalf("Current = %p, want a fresh member", replacement)
	}
}

// TestScheduleReplacementLeavesALiveKeyAlone pins the other reading the
// watcher must not make: a key whose member is live and was never
// invalidated has nothing to replace, so arming it builds nothing.
func TestScheduleReplacementLeavesALiveKeyAlone(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/live"}
	m := h.pooled(k, 0)

	if !h.pool.ScheduleReplacement(context.Background(), k) {
		t.Fatal("the replacement was not armed")
	}
	h.eventually("the armed slot to be released", func() bool {
		return !h.pool.ReplacementArmed(k)
	})
	if got := h.buildCount(); got != 0 {
		t.Fatalf("assemblies = %d, want 0: a live member serves the key", got)
	}
	if got := h.pool.Current(k); got != m {
		t.Fatalf("Current = %p, want the live %p", got, m)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.replaced) != 0 {
		t.Fatalf("Replaced ran for %v, want nothing", h.replaced)
	}
}

// TestScheduleReplacementSkipsAKeyNobodyWants pins the Wanted hook: a
// key the application left while the old member drained is not rebuilt
// behind the user's back, and the armed slot is still released.
func TestScheduleReplacementSkipsAKeyNobodyWants(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/left"}
	first := h.pooled(k, 0)
	h.mu.Lock()
	h.wanted = func(Key) bool { return false }
	h.mu.Unlock()

	h.pool.Invalidate(context.Background(), k)
	h.settleRetirement(k, first)

	if !h.pool.ScheduleReplacement(context.Background(), k) {
		t.Fatal("the replacement was not armed")
	}
	h.eventually("the armed slot to be released", func() bool {
		return !h.pool.ReplacementArmed(k)
	})
	if got := h.buildCount(); got != 0 {
		t.Fatalf("assemblies = %d, want 0: the key is not wanted", got)
	}
	if got := h.pool.Current(k); got != nil {
		t.Fatalf("Current = %p, want the key left unserved", got)
	}
}

// TestDeferredRebuildFailureIsReported pins that a replacement that
// cannot be assembled is not silent: the watcher has nobody to return
// to, so the failure is what the log is for.
func TestDeferredRebuildFailureIsReported(t *testing.T) {
	sink := installLogSink(t)
	h := newHarness(t)
	k := Key{"workspace", "/ws/rebuild-failed"}
	first := h.pooled(k, 0)
	buildErr := errors.New("assembly refused")
	h.openErrSet(buildErr)

	h.pool.Invalidate(context.Background(), k)
	h.settleRetirement(k, first)
	if !h.pool.ScheduleReplacement(context.Background(), k) {
		t.Fatal("the replacement was not armed")
	}
	h.eventually("the armed slot to be released", func() bool {
		return !h.pool.ReplacementArmed(k)
	})

	if records := sink.find("pool: deferred rebuild failed"); len(records) != 1 {
		t.Fatalf("deferred-rebuild failure lines = %d, want 1", len(records))
	} else if got := records[0].attrs["key"]; got != k.String() {
		t.Fatalf("key = %q, want %q", got, k.String())
	}
}
