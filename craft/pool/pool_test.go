package pool

import (
	"context"
	"errors"
	"sync"

	"testing"
	"time"
)

// TestAcquireSharesOneAssembly pins the single-flight contract behind a
// rebuild storm: a burst of Acquire calls for one key runs one
// assembly, and every caller receives the same member with its own
// reference. Before this, each waker built a member and all but the
// first were closed again.
func TestAcquireSharesOneAssembly(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.mu.Lock()
	h.openGate = release
	h.mu.Unlock()

	k := Key{"workspace", "/ws/single-flight"}
	const callers = 5
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		members []*fakeMember
		errs    []error
		started = make(chan struct{})
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-started
			m, err := h.pool.Acquire(context.Background(), k)
			mu.Lock()
			defer mu.Unlock()
			members = append(members, m)
			errs = append(errs, err)
		}()
	}
	close(started)
	// Give every caller time to reach the in-flight assembly before the
	// leader is allowed to finish, so they all wait on the same call.
	h.eventually("the assembly to start", func() bool {
		return h.buildCount() > 0 && h.pendingAssemblies() > 0
	})
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := h.buildCount(); got != 1 {
		t.Fatalf("assemblies = %d, want 1 (concurrent Acquire must share)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if len(members) != callers {
		t.Fatalf("members returned = %d, want %d", len(members), callers)
	}
	for i, m := range members {
		if m != members[0] {
			t.Fatalf("caller %d got member %p, want %p", i, m, members[0])
		}
	}
	h.pool.mu.Lock()
	refs := h.pool.pooled[k].refs
	h.pool.mu.Unlock()
	if refs != callers {
		t.Fatalf("pooled refs = %d, want %d", refs, callers)
	}
}

// TestAcquireRetriesAfterFailedAssembly pins the other half of the
// contract: a failed assembly is shared with the callers that were
// already waiting (so one broken configuration is reported once), and
// the next Acquire retries instead of inheriting the failure forever.
func TestAcquireRetriesAfterFailedAssembly(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.mu.Lock()
	h.openGate = release
	h.mu.Unlock()
	buildErr := errors.New("assembly refused")
	h.openErrSet(buildErr)

	k := Key{"workspace", "/ws/retry"}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.pool.Acquire(context.Background(), k)
		}(i)
	}
	// Let both callers land inside the one in-flight assembly before it
	// is allowed to fail, so the failure is shared rather than run
	// twice.
	h.eventually("the shared assembly to start", func() bool {
		return h.buildCount() > 0 && h.pendingAssemblies() > 0
	})
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, buildErr) {
			t.Fatalf("caller %d error = %v, want %v", i, err, buildErr)
		}
	}
	if got := h.buildCount(); got != 1 {
		t.Fatalf("assemblies = %d, want 1 shared failure", got)
	}
	if h.pendingAssemblies() != 0 {
		t.Fatal("failed assembly left an in-flight entry behind")
	}

	// The failure is not sticky: the next caller assembles again.
	h.openErrSet(nil)
	m := h.mustAcquire(k)
	if m == nil {
		t.Fatal("retry after failure returned no member")
	}
	if got := h.buildCount(); got != 2 {
		t.Fatalf("assemblies = %d, want 2 (one failed, one retried)", got)
	}
}

// TestInstalledRunsOncePerPooledMember pins the contract the
// application's wiring depends on: the hook runs when a member is
// handed out, once per member rather than once per call, and the marker
// rides the pool entry — so a member the pool has forgotten stops being
// referenced by the bookkeeping that wired it.
func TestInstalledRunsOncePerPooledMember(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/configure-once"}

	first := h.mustAcquire(k)
	second := h.mustAcquire(k)
	if second != first {
		t.Fatalf("second acquire returned %p, want the pooled %p", second, first)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.installed) != 1 {
		t.Fatalf("Installed ran %d times for one pooled member, want 1",
			len(h.installed))
	}
}

// TestEnsureWiresAMemberThePoolAlreadyHeld pins the other half: no
// hand-out path returns a member that can serve work unwired. Ensure's
// pooled branch is the one that could — it returns a member the
// assembler has not reached yet (Installed runs after publishing), and
// one that was pooled before the hook ran is reachable the same way.
func TestEnsureWiresAMemberThePoolAlreadyHeld(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/configure-on-ensure"}
	pooled := h.pooled(k, 0)

	got := h.mustEnsure(k)
	if got != pooled {
		t.Fatalf("ensure returned %p, want the pooled %p", got, pooled)
	}
	h.mu.Lock()
	installed := append([]Key(nil), h.installed...)
	h.mu.Unlock()
	if len(installed) != 1 || installed[0] != k {
		t.Fatalf("Installed ran for %v, want once for %s", installed, k)
	}

	if _, err := h.pool.Ensure(context.Background(), k); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.installed) != 1 {
		t.Fatalf("Installed ran %d times for one pooled member, want 1",
			len(h.installed))
	}
}

// TestReloadDefersUntilTheWorkEnds pins the reload-during-work
// semantics the two entrance points exist for:
//
//  1. a member has work in flight;
//  2. an invalidation marks it stale but keeps it pooled and serving;
//  3. Ensure and Acquire hand the same member back — the work continues
//     on the generation it started on, and no second member is
//     assembled under it;
//  4. once the work ends, the member retires itself and the next Ensure
//     assembles a fresh one.
func TestReloadDefersUntilTheWorkEnds(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/reload-during-work"}
	first := h.pooled(k, 0)
	first.work()

	ctx := WithReason(context.Background(), "settings_save")
	h.pool.Invalidate(ctx, k)

	if got := h.pool.Current(k); got != first {
		t.Fatalf("Current = %p, want the serving member %p", got, first)
	}
	if !h.pool.Stale(k) {
		t.Fatal("the invalidated member is not reported stale")
	}
	h.assertNothingClosed()

	if got := h.mustEnsure(k); got != first {
		t.Fatalf("Ensure = %p, want the stale-but-serving %p", got, first)
	}
	if got := h.mustAcquire(k); got != first {
		t.Fatalf("Acquire = %p, want the stale-but-serving %p", got, first)
	}
	if h.buildCount() != 0 {
		t.Fatalf("assemblies = %d, want 0: nothing may be built under "+
			"a live unit of work", h.buildCount())
	}

	// The old member must still be usable while the work is in flight.
	if first.isClosed() {
		t.Fatal("the member was torn down under a live unit of work")
	}

	h.finish(first)
	h.waitClosed(first)
	h.eventually("the retired member to leave the pool", func() bool {
		return h.pool.Current(k) == nil
	})
	if !h.pool.Stale(k) {
		t.Fatal("the retired generation stopped being reported stale")
	}

	fresh := h.mustEnsure(k)
	if fresh == first {
		t.Fatal("the retired member was handed out again")
	}
	if h.pool.Stale(k) {
		t.Fatal("a freshly assembled member is still reported stale")
	}
}

// TestAcquireWaitsOutARetiringMember pins the Acquire half of the two
// entrance points: a caller that arrives while the previous generation
// is still tearing down waits for that teardown — it is never handed a
// member that is on its way out, and no second member is assembled for
// the key meanwhile.
func TestAcquireWaitsOutARetiringMember(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/drain"}
	old := h.pooled(k, 0)
	gate := make(chan struct{})
	h.mu.Lock()
	h.closeGate = gate
	h.mu.Unlock()

	h.pool.Invalidate(context.Background(), k)
	h.waitClosed(old)

	type result struct {
		m   *fakeMember
		err error
	}
	got := make(chan result, 1)
	go func() {
		m, err := h.pool.Acquire(context.Background(), k)
		got <- result{m, err}
	}()

	time.Sleep(50 * time.Millisecond)
	if h.buildCount() != 0 {
		t.Fatal("a second member was assembled while the old one drained")
	}
	select {
	case r := <-got:
		t.Fatalf("acquire returned %p, %v; want it to wait for the teardown",
			r.m, r.err)
	default:
	}

	close(gate)
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("acquire after teardown: %v", r.err)
		}
		if r.m == old {
			t.Fatal("acquire returned the retired member")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire never returned after the teardown finished")
	}
}

// TestInvalidateClosesIdleMember pins the immediate half of the
// invalidation contract: nothing is in flight, so the member leaves the
// pool and its teardown begins now.
func TestInvalidateClosesIdleMember(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/idle"}
	m := h.pooled(k, 0)

	h.pool.Invalidate(context.Background(), k)

	h.settleRetirement(k, m)
	if h.pool.Current(k) != nil {
		t.Fatal("an idle member stayed reachable after invalidation")
	}
	if !m.isClosed() {
		t.Fatal("the member was not closed")
	}
	if !h.pool.Stale(k) {
		t.Fatal("the invalidated key is not reported stale")
	}
}

// TestInvalidateDefersBusyMember pins the deferred half: a member with
// work in flight stays pooled — a second generation must never be
// assembled under a live unit — and only the eventual idle retires it.
func TestInvalidateDefersBusyMember(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/busy"}
	m := h.pooled(k, 1)

	h.pool.Invalidate(context.Background(), k)

	if got := h.pool.Current(k); got != m {
		t.Fatalf("Current = %p, want the still-serving %p", got, m)
	}
	h.assertNothingClosed()
	if m.isClosed() {
		t.Fatal("the member was closed while it still had work in flight")
	}

	// The last unit ending is what retires it.
	h.finish(m)
	h.settleRetirement(k, m)
	if h.pool.Current(k) != nil {
		t.Fatal("the retired member is still reachable")
	}
}

// TestSettleIgnoresMemberThatWasNotInvalidated pins that becoming idle
// is not itself a retirement: nothing is torn down, and the key goes on
// being served by the same generation.
func TestSettleIgnoresMemberThatWasNotInvalidated(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/idle-not-stale"}
	m := h.pooled(k, 1)

	h.finish(m)

	if got := h.pool.Current(k); got != m {
		t.Fatalf("Current = %p, want the pooled %p", got, m)
	}
	h.assertNothingClosed()
	if m.isClosed() {
		t.Fatal("a member that was never invalidated was closed at idle")
	}
}

// TestLateSettleDoesNotRetireTheSuccessor pins the pairing in Settle: a
// settle that arrives after its member was already retired — a late
// "I am idle now" from the previous generation — must not take the
// generation that replaced it down with it, even when that successor is
// itself stale and would retire at its own idle.
func TestLateSettleDoesNotRetireTheSuccessor(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/late-settle"}
	old := h.pooled(k, 1)

	h.pool.Invalidate(context.Background(), k)
	h.finish(old)
	h.settleRetirement(k, old)

	fresh := h.mustEnsure(k)
	if fresh == old {
		t.Fatal("the retired member was handed out again")
	}
	// The successor picks up work of its own and is invalidated behind
	// it: it is stale, pooled, and serving.
	fresh.work()
	h.pool.Invalidate(context.Background(), k)

	// The late settle names the retired generation.
	h.pool.Settle(k, old)

	if got := h.pool.Current(k); got != fresh {
		t.Fatalf("Current = %p, want the successor %p", got, fresh)
	}
	h.assertNothingClosed()
	if fresh.isClosed() {
		t.Fatal("a late settle retired the successor")
	}

	// Its own idle is what retires it.
	h.finish(fresh)
	h.settleRetirement(k, fresh)
}

// TestReleaseRetiresAtTheLastReference pins the reference contract:
// Acquire checks a member out and Release is what retires it, once the
// last caller is done.
// TestEnsureTakesNoReference pins the other half of the reference
// contract: Ensure borrows the generation that serves the key — it is
// the read path, called once per unit of work — so the reference a
// publication carries stays the only one, and one Release retires the
// member.
func TestEnsureTakesNoReference(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/ensure-borrow"}
	m := h.pooled(k, 0)
	h.mustEnsure(k)
	if got := h.pooledEntry(k); got != m {
		t.Fatalf("Ensure = %p, want the pooled %p", got, m)
	}
	h.pool.Release(k, m)
	h.settleRetirement(k, m)
}

func TestReleaseRetiresAtTheLastReference(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/refs"}
	// The pool's own reference is the one a publication carries.
	m := h.pooled(k, 0)

	h.mustAcquire(k)
	h.pool.Release(k, m)
	if got := h.pool.Current(k); got != m {
		t.Fatalf("Current = %p, want the still-referenced %p", got, m)
	}
	h.assertNothingClosed()

	h.pool.Release(k, m)
	h.settleRetirement(k, m)
	if h.pool.Current(k) != nil {
		t.Fatal("the released member is still reachable")
	}
	if h.pool.Stale(k) {
		t.Fatal("a released member is not stale: nobody is waiting for it")
	}

	fresh := h.mustEnsure(k)
	if fresh == m {
		t.Fatal("the released member was handed out again")
	}
}

// TestWaitClosedHonorsContextCancel pins that a caller waiting for a
// teardown is bounded by its own context.
func TestWaitClosedHonorsContextCancel(t *testing.T) {
	m := newFakeMember(Key{"workspace", "/ws/wait"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.waitClosed(ctx); err == nil {
		t.Fatal("waitClosed returned nil on a canceled context")
	}
}

// TestAssemblyThatReturnsNoMemberIsReported pins the guard around an
// Open hook that answers with (nil, nil): it is reported instead of
// publishing a member the pool cannot hand out or close.
func TestAssemblyThatReturnsNoMemberIsReported(t *testing.T) {
	h := newHarness(t, func(spec *Spec[fakeMember]) {
		spec.Open = func(context.Context, Key) (*fakeMember, error) {
			return nil, nil
		}
	})
	_, err := h.pool.Acquire(context.Background(), Key{"workspace", "/ws/nil"})
	if !errors.Is(err, ErrNoMember) {
		t.Fatalf("Acquire = %v, want ErrNoMember", err)
	}
}

// TestInvalidateLogsAttribution pins the diagnostic contract the
// invalidation line exists for: it names the caller that asked for the
// rebuild, the key it tore down, and whether the swap was deferred
// behind live work. A rebuild storm has to be answerable from the log
// alone.
func TestInvalidateLogsAttribution(t *testing.T) {
	sink := installLogSink(t)
	h := newHarness(t)
	k := Key{"workspace", "/ws/logged"}
	h.pooled(k, 1)

	ctx := WithReason(context.Background(), "settings_save")
	h.pool.Invalidate(ctx, k)

	records := sink.find("pool: member invalidated")
	if len(records) != 1 {
		t.Fatalf("invalidation lines = %d, want 1", len(records))
	}
	got := records[0].attrs
	if got["reason"] != "settings_save" {
		t.Fatalf("reason = %q, want settings_save", got["reason"])
	}
	if got["key"] != "workspace=/ws/logged" {
		t.Fatalf("key = %q, want workspace=/ws/logged", got["key"])
	}
	if got["in_turn"] != "true" {
		t.Fatalf("in_turn = %q, want true", got["in_turn"])
	}
	if got["deferred"] != "true" {
		t.Fatalf("deferred = %q, want true", got["deferred"])
	}
	if got["member_ptr"] == "" {
		t.Fatal("the invalidation line names no member")
	}
}

// TestAssemblyFailureIsLogged pins the other diagnostic: a failed
// assembly names its key and its reason, so "opens are slow" and "this
// key was built four times" can be told apart from the log.
func TestAssemblyFailureIsLogged(t *testing.T) {
	sink := installLogSink(t)
	h := newHarness(t)
	buildErr := errors.New("assembly refused")
	h.openErrSet(buildErr)
	k := Key{"workspace", "/ws/failing"}

	_, err := h.pool.Ensure(
		WithReason(context.Background(), "plugin_change"), k)
	if !errors.Is(err, buildErr) {
		t.Fatalf("Ensure = %v, want %v", err, buildErr)
	}

	records := sink.find("pool: member assembly failed")
	if len(records) != 1 {
		t.Fatalf("assembly-failure lines = %d, want 1", len(records))
	}
	got := records[0].attrs
	if got["reason"] != "plugin_change" {
		t.Fatalf("reason = %q, want plugin_change", got["reason"])
	}
	if got["key"] != "workspace=/ws/failing" {
		t.Fatalf("key = %q, want workspace=/ws/failing", got["key"])
	}
	if got["error.message"] != buildErr.Error() {
		t.Fatalf("error = %q, want %q", got["error.message"], buildErr)
	}
}

// TestAssemblyIsLoggedWithItsSequence pins the rebuild-storm signal:
// every assembly logs one line naming its reason and the key's sequence
// number, so "this key was assembled four times while I opened one
// panel" is answerable from the log alone.
func TestAssemblyIsLoggedWithItsSequence(t *testing.T) {
	sink := installLogSink(t)
	h := newHarness(t)
	k := Key{"workspace", "/ws/storm"}

	for i := 1; i <= 2; i++ {
		ctx := WithReason(context.Background(), "plugin_change")
		m := h.mustEnsureWith(ctx, k)
		h.pool.Invalidate(ctx, k)
		h.settleRetirement(k, m)
	}

	records := sink.find("pool: member assembled")
	if len(records) != 2 {
		t.Fatalf("assembly lines = %d, want 2", len(records))
	}
	for i, want := range []string{"1", "2"} {
		if got := records[i].attrs["assembly_seq"]; got != want {
			t.Fatalf("line %d: assembly_seq = %q, want %q", i, got, want)
		}
		if got := records[i].attrs["reason"]; got != "plugin_change" {
			t.Fatalf("line %d: reason = %q, want plugin_change", i, got)
		}
		if got := records[i].attrs["key"]; got != k.String() {
			t.Fatalf("line %d: key = %q, want %q", i, got, k.String())
		}
	}
}

// TestCloseRetiresEveryMember pins the pool-wide verb: idle members
// close now, members with work in flight close after it ends, and the
// pool keeps working afterwards — which is what the reload path needs
// from it.
func TestCloseRetiresEveryMember(t *testing.T) {
	h := newHarness(t)
	idle := Key{"workspace", "/ws/idle"}
	busy := Key{"app", "demo"}
	idleMember := h.pooled(idle, 0)
	busyMember := h.pooled(busy, 1)

	h.pool.Close()

	h.settleRetirement(idle, idleMember)
	h.assertNothingClosed()
	if h.pool.Current(idle) != nil {
		t.Fatal("the idle member is still reachable after Close")
	}
	if got := h.pool.Current(busy); got != busyMember {
		t.Fatalf("Current = %p, want the busy %p", got, busyMember)
	}
	h.finish(busyMember)
	h.waitClosed(busyMember)

	fresh := h.mustEnsure(idle)
	if fresh == idleMember {
		t.Fatal("the pool did not assemble again after Close")
	}
}

// TestConcurrentAcquireInvalidateAndRelease exercises the bookkeeping
// from every direction at once — acquisitions, releases and
// invalidations racing on one key — under the race detector. What it
// pins is the invariant the pool exists to keep: whatever the
// interleaving, the key ends up with no member at all once every
// reference went away, and none of the callers was left holding a
// promise the pool never kept.
func TestConcurrentAcquireInvalidateAndRelease(t *testing.T) {
	h := newHarness(t)
	k := Key{"workspace", "/ws/racing"}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := h.pool.Acquire(context.Background(), k)
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
			if err != nil {
				return
			}
			h.pool.Release(k, m)
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.pool.Invalidate(context.Background(), k)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	// Nothing is stuck: every generation retired, and no member is left
	// behind for the key.
	h.eventually("the key to settle", func() bool { return h.retired(k) })
	if got := h.pool.Current(k); got != nil {
		t.Fatalf("Current = %p, want no member once every reference went away", got)
	}
}

// TestOpenGateHonorsContextCancel pins that a caller waiting for a
// shared assembly is bounded by its own context.
func TestOpenGateHonorsContextCancel(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	defer close(release)
	h.mu.Lock()
	h.openGate = release
	h.mu.Unlock()

	k := Key{"workspace", "/ws/cancel-assembly"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := h.pool.Acquire(ctx, k); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
}
