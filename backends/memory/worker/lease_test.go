package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// leaseClock is a clock a test moves by hand: an expiry is a comparison, so a
// test that has to watch a claim stop being one moves the clock instead of
// waiting for it.
type leaseClock struct {
	mu  sync.Mutex
	now time.Time
}

func newLeaseClock(now time.Time) *leaseClock {
	return &leaseClock{now: now}
}

func (clock *leaseClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *leaseClock) Advance(delta time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(delta)
}

func newTestLeaseStore(t *testing.T, clock *leaseClock) (*LeaseStore, storage.Store) {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewLeaseStore(kv, WithLeaseClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	return store, kv
}

// TestLeaseHandsOneScopeToOneHolder pins the exclusion a lease buys: while one
// holder's claim lives, another holder is refused and told who holds it; the
// release frees the scope for the next acquirer.
func TestLeaseHandsOneScopeToOneHolder(t *testing.T) {
	ctx := context.Background()
	clock := newLeaseClock(testLeaseNow())
	store, _ := newTestLeaseStore(t, clock)
	scope := corememory.Scope{RuntimeID: "memories", UserID: "u1"}

	first, held, err := store.Acquire(ctx, scope, "runner-a", time.Minute)
	if err != nil || !held {
		t.Fatalf("first acquire = %#v, %v, %v", first, held, err)
	}
	second, held, err := store.Acquire(ctx, scope, "runner-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatalf("second holder took a scope that was held: %#v", second)
	}
	if second.Holder != "runner-a" {
		t.Fatalf("refused acquire reported holder %q, want runner-a", second.Holder)
	}
	if released, err := store.Release(ctx, first); err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	third, held, err := store.Acquire(ctx, scope, "runner-b", time.Minute)
	if err != nil || !held {
		t.Fatalf("acquire after release = %#v, %v, %v", third, held, err)
	}
	if third.Holder != "runner-b" {
		t.Fatalf("acquired holder = %q, want runner-b", third.Holder)
	}
}

// TestLeaseReacquireByTheHolderIsIdempotent pins that asking twice is not a
// conflict: the scope is already the holder's, and a claim keeps the time it
// was first taken.
func TestLeaseReacquireByTheHolderIsIdempotent(t *testing.T) {
	ctx := context.Background()
	clock := newLeaseClock(testLeaseNow())
	store, _ := newTestLeaseStore(t, clock)
	scope := corememory.Scope{RuntimeID: "memories"}

	first, held, err := store.Acquire(ctx, scope, "runner-a", time.Minute)
	if err != nil || !held {
		t.Fatalf("first acquire = %#v, %v, %v", first, held, err)
	}
	clock.Advance(10 * time.Second)
	second, held, err := store.Acquire(ctx, scope, "runner-a", time.Minute)
	if err != nil || !held {
		t.Fatalf("second acquire = %#v, %v, %v", second, held, err)
	}
	if !second.AcquiredAt.Equal(first.AcquiredAt) {
		t.Fatalf("acquired at moved from %v to %v", first.AcquiredAt, second.AcquiredAt)
	}
	if !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("expiry moved from %v to %v without a renewal", first.ExpiresAt, second.ExpiresAt)
	}
}

// TestLeaseRenewalKeepsAWorkingHolder pins the renewal: a claim that is renewed
// outlives the ttl it was taken with, and the holder that renewed it keeps the
// scope while a second holder is refused for the extended claim.
func TestLeaseRenewalKeepsAWorkingHolder(t *testing.T) {
	ctx := context.Background()
	clock := newLeaseClock(testLeaseNow())
	store, _ := newTestLeaseStore(t, clock)
	scope := corememory.Scope{RuntimeID: "memories"}

	lease, held, err := store.Acquire(ctx, scope, "runner-a", time.Minute)
	if err != nil || !held {
		t.Fatalf("acquire = %#v, %v, %v", lease, held, err)
	}
	clock.Advance(50 * time.Second)
	renewed, held, err := store.Renew(ctx, lease, time.Minute)
	if err != nil || !held {
		t.Fatalf("renew = %#v, %v, %v", renewed, held, err)
	}
	if !renewed.AcquiredAt.Equal(lease.AcquiredAt) {
		t.Fatalf("renewal moved the acquisition time to %v", renewed.AcquiredAt)
	}
	if !renewed.ExpiresAt.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("renewed expiry = %v", renewed.ExpiresAt)
	}
	clock.Advance(30 * time.Second)
	if _, taken, err := store.Acquire(ctx, scope, "runner-b", time.Minute); err != nil || taken {
		t.Fatalf("acquire past the original ttl = %v, %v", taken, err)
	}
	clock.Advance(20 * time.Second)
	if _, taken, err := store.Acquire(ctx, scope, "runner-b", time.Minute); err != nil || taken {
		t.Fatalf("acquire past the renewed ttl = %v, %v", taken, err)
	}
}

// TestLeaseExpiresAndIsStolen pins the bound a claim has: past its expiry it
// stops being one, a second holder takes it, and the holder that lost it can
// neither renew nor release what is no longer its own.
func TestLeaseExpiresAndIsStolen(t *testing.T) {
	ctx := context.Background()
	clock := newLeaseClock(testLeaseNow())
	store, _ := newTestLeaseStore(t, clock)
	scope := corememory.Scope{RuntimeID: "memories"}

	lease, held, err := store.Acquire(ctx, scope, "runner-a", 30*time.Second)
	if err != nil || !held {
		t.Fatalf("acquire = %#v, %v, %v", lease, held, err)
	}
	clock.Advance(31 * time.Second)
	if current, found, err := store.Current(ctx, scope); err != nil || !found || !current.Expired(clock.Now()) {
		t.Fatalf("expired claim = %#v, %v, %v", current, found, err)
	}
	stolen, held, err := store.Acquire(ctx, scope, "runner-b", time.Minute)
	if err != nil || !held {
		t.Fatalf("acquire of an expired claim = %#v, %v, %v", stolen, held, err)
	}
	if stolen.Holder != "runner-b" {
		t.Fatalf("holder after the steal = %q", stolen.Holder)
	}
	if _, held, err := store.Renew(ctx, lease, time.Minute); err != nil || held {
		t.Fatalf("the holder that lost the scope renewed it: %v, %v", held, err)
	}
	if released, err := store.Release(ctx, lease); err != nil || released {
		t.Fatalf("the holder that lost the scope released it: %v, %v", released, err)
	}
	if released, err := store.Release(ctx, stolen); err != nil || !released {
		t.Fatalf("the holder released its own claim: %v, %v", released, err)
	}
}

// TestLeaseReleaseLeavesAReadableClaim pins what a release writes: the scope is
// free, and the claim stays readable so a host can see who last held it rather
// than an empty key.
func TestLeaseReleaseLeavesAReadableClaim(t *testing.T) {
	ctx := context.Background()
	clock := newLeaseClock(testLeaseNow())
	store, _ := newTestLeaseStore(t, clock)
	scope := corememory.Scope{RuntimeID: "memories"}

	lease, _, err := store.Acquire(ctx, scope, "runner-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	current, found, err := store.Current(ctx, scope)
	if err != nil || !found {
		t.Fatalf("claim after release = %#v, %v, %v", current, found, err)
	}
	if !current.Released || current.Held() || !current.Expired(clock.Now()) {
		t.Fatalf("released claim = %#v", current)
	}
	if current.Holder != "runner-a" {
		t.Fatalf("released claim holder = %q", current.Holder)
	}
}

// TestLeaseIsPerScope pins that a claim is one scope's: two scopes of the same
// store are held at the same time, which is what lets one runner derive two
// scopes and two runners derive different ones.
func TestLeaseIsPerScope(t *testing.T) {
	ctx := context.Background()
	clock := newLeaseClock(testLeaseNow())
	store, _ := newTestLeaseStore(t, clock)
	first := corememory.Scope{RuntimeID: "memories", UserID: "u1"}
	second := corememory.Scope{RuntimeID: "memories", UserID: "u2"}

	if _, held, err := store.Acquire(ctx, first, "runner-a", time.Minute); err != nil || !held {
		t.Fatalf("acquire first = %v, %v", held, err)
	}
	if _, held, err := store.Acquire(ctx, second, "runner-b", time.Minute); err != nil || !held {
		t.Fatalf("acquire second = %v, %v", held, err)
	}
	if _, held, err := store.Acquire(ctx, first, "runner-b", time.Minute); err != nil || held {
		t.Fatalf("second runner took the first scope: %v, %v", held, err)
	}
}

// TestLeaseRefusesUnusableInputs pins the refusals a caller has to see rather
// than a claim written for something that cannot hold one.
func TestLeaseRefusesUnusableInputs(t *testing.T) {
	ctx := context.Background()
	clock := newLeaseClock(testLeaseNow())
	store, _ := newTestLeaseStore(t, clock)
	scope := corememory.Scope{RuntimeID: "memories"}

	if _, _, err := store.Acquire(ctx, scope, "  ", time.Minute); err == nil {
		t.Fatal("acquire accepted an empty holder")
	}
	if _, _, err := store.Acquire(ctx, scope, "runner-a", 0); err == nil {
		t.Fatal("acquire accepted a zero ttl")
	}
	if _, _, err := store.Acquire(ctx, scope, "runner-a", -time.Second); err == nil {
		t.Fatal("acquire accepted a negative ttl")
	}
	if _, _, err := store.Acquire(ctx, corememory.Scope{}, "runner-a", time.Minute); err == nil {
		t.Fatal("acquire accepted an invalid scope")
	}
	if _, err := store.Release(ctx, ScopeLease{Scope: scope, Holder: "runner-a"}); err != nil {
		t.Fatalf("release of a claim that was never stored = %v, want no error and nothing released", err)
	}
	if _, _, err := store.Renew(ctx, ScopeLease{Scope: scope}, time.Minute); err == nil {
		t.Fatal("renew accepted a claim with no holder")
	}
}

// TestLeaseRequiresCheckAndWriteStorage pins the wiring requirement: a store
// that cannot decide a race cannot hand out a lease, and saying so at
// construction is better than racing later.
func TestLeaseRequiresCheckAndWriteStorage(t *testing.T) {
	_, err := NewLeaseStore(plainKV{})
	if err == nil {
		t.Fatal("a store without compare-and-swap was accepted")
	}
	if _, err := NewLeaseStore(nil); err == nil {
		t.Fatal("a missing store was accepted")
	}
}

// TestLeaseRefusesACorruptClaim pins that a claim nobody can read is an error
// rather than something a runner decides about.
func TestLeaseRefusesACorruptClaim(t *testing.T) {
	ctx := context.Background()
	clock := newLeaseClock(testLeaseNow())
	store, kv := newTestLeaseStore(t, clock)
	scope := corememory.Scope{RuntimeID: "memories"}
	key, err := leaseKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, key, []byte("{\"holder\":\"runner-a\"}")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Acquire(ctx, scope, "runner-b", time.Minute); err == nil {
		t.Fatal("acquire accepted a claim with no expiry")
	}
	if err := kv.Put(ctx, key, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Acquire(ctx, scope, "runner-b", time.Minute); err == nil {
		t.Fatal("acquire accepted an unreadable claim")
	}
}

// plainKV is a storage.Store without the check-and-write extensions.
type plainKV struct{}

func (plainKV) Get(context.Context, string) ([]byte, error) { return nil, storage.ErrNotFound }
func (plainKV) Put(context.Context, string, []byte) error   { return errors.New("plain kv: put") }
func (plainKV) Delete(context.Context, string) error        { return errors.New("plain kv: delete") }
func (plainKV) List(context.Context, string) ([]storage.Entry, error) {
	return nil, errors.New("plain kv: list")
}

func testLeaseNow() time.Time {
	return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
}
