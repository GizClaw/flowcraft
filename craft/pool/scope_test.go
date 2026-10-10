package pool

import (
	"context"
	"errors"
	"testing"
)

// scopeFixture pools one workspace and two applications in one pool:
// the smallest pool in which "just this kind" and "every member of this
// kind" are distinguishable from "everything" and from "nothing".
type scopeFixture struct {
	h     *harness
	ws    *fakeMember
	demo  *fakeMember
	other *fakeMember
}

func newScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	h := newHarness(t)
	return scopeFixture{
		h:     h,
		ws:    h.pooled(Key{"workspace", "/ws/a"}, 0),
		demo:  h.pooled(Key{"app", "demo"}, 0),
		other: h.pooled(Key{"app", "other"}, 0),
	}
}

// TestInvalidateKindLeavesOtherKindsAlone pins the split the one
// invalidate-everything used to miss: a workspace reload must leave
// every application's member alone. Both kinds share one pool, so the
// only thing keeping them apart is the key's kind.
func TestInvalidateKindLeavesOtherKindsAlone(t *testing.T) {
	f := newScopeFixture(t)
	f.h.pool.InvalidateKind(context.Background(), "workspace")

	f.h.waitRetired(f.ws)
	f.h.assertClosedExactly(f.ws)
	f.h.assertPooled(f.demo, f.other)
}

// TestInvalidateKindDropsEveryMemberOfThatKind pins the other
// direction: a change that is application-shaped — a settings save, an
// application update — retires every application and no workspace.
func TestInvalidateKindDropsEveryMemberOfThatKind(t *testing.T) {
	f := newScopeFixture(t)
	f.h.pool.InvalidateKind(context.Background(), "app")

	f.h.waitRetired(f.demo)
	f.h.waitRetired(f.other)
	f.h.assertClosedExactly(f.demo, f.other)
	f.h.assertPooled(f.ws)
}

// TestInvalidateAllAndCloseRetireEveryKind pins the union the two
// single-kind calls were split out of. It is on the live path: Close is
// what a shutdown calls, so a version that quietly covered one kind
// would leak every other kind's member, with nothing else in the suite
// to say so.
func TestInvalidateAllAndCloseRetireEveryKind(t *testing.T) {
	t.Run("InvalidateAll", func(t *testing.T) {
		f := newScopeFixture(t)
		f.h.pool.InvalidateAll(context.Background())
		for _, m := range []*fakeMember{f.ws, f.demo, f.other} {
			f.h.waitRetired(m)
		}
		f.h.assertClosedExactly(f.ws, f.demo, f.other)
	})

	t.Run("Close", func(t *testing.T) {
		f := newScopeFixture(t)
		f.h.pool.Close()
		for _, m := range []*fakeMember{f.ws, f.demo, f.other} {
			f.h.waitRetired(m)
		}
		f.h.assertClosedExactly(f.ws, f.demo, f.other)
	})
}

// TestTheSameIDIsTwoKeys pins why the key carries the kind: a directory
// named exactly like an application must resolve to its own member, and
// neither kind may answer for the other. Retiring one must not take the
// other's entry with it.
func TestTheSameIDIsTwoKeys(t *testing.T) {
	h := newHarness(t)
	same := "/apps/werewolf"
	ws := h.pooled(Key{"workspace", same}, 0)
	app := h.pooled(Key{"app", same}, 0)

	if got := h.pool.Current(Key{"workspace", same}); got != ws {
		t.Fatalf("workspace Current = %p, want %p", got, ws)
	}
	if got := h.pool.Current(Key{"app", same}); got != app {
		t.Fatalf("app Current = %p, want %p", got, app)
	}
	h.pool.mu.Lock()
	entries := len(h.pool.pooled)
	h.pool.mu.Unlock()
	if entries != 2 {
		t.Fatalf("pool holds %d entries for two keys with one id", entries)
	}

	h.pool.InvalidateKind(context.Background(), "workspace")
	h.waitRetired(ws)
	if got := h.pool.Current(Key{"app", same}); got != app {
		t.Fatalf("app Current after a workspace reload = %p, want %p", got, app)
	}
	h.assertClosedExactly(ws)
}

// TestUnnamedKeyIsRefused pins the guard the key shape brings: an empty
// kind or id would otherwise assemble (and pool) a member for whatever
// the application cleans it to. Asking for a member means naming a key,
// and the refusal comes before the assembler runs at all.
func TestUnnamedKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		key  Key
	}{
		{"zero", Key{}},
		{"blank kind", Key{ID: "/ws/a"}},
		{"blank id", Key{Kind: "workspace"}},
		{"blank spaces", Key{Kind: " ", ID: " "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.pool.Acquire(context.Background(), tc.key); !errors.Is(err, ErrNoKey) {
				t.Fatalf("Acquire = %v, want ErrNoKey", err)
			}
			if _, err := h.pool.Ensure(context.Background(), tc.key); !errors.Is(err, ErrNoKey) {
				t.Fatalf("Ensure = %v, want ErrNoKey", err)
			}
			if got := h.pool.Current(tc.key); got != nil {
				t.Fatalf("Current = %p, want no member for an unnamed key", got)
			}
			if h.pool.Stale(tc.key) {
				t.Fatal("an unnamed key was reported stale")
			}
			if h.pool.ReplacementArmed(tc.key) {
				t.Fatal("an unnamed key was reported armed")
			}
			// The mutations ignore it: there is nothing to retire, and
			// nothing to arm.
			h.pool.Invalidate(context.Background(), tc.key)
			if h.pool.ScheduleReplacement(context.Background(), tc.key) {
				t.Fatal("an unnamed key was armed for a replacement")
			}
			h.pool.Release(tc.key, newFakeMember(tc.key))
			h.pool.Settle(tc.key, newFakeMember(tc.key))
		})
	}
	if got := h.buildCount(); got != 0 {
		t.Fatalf("an unnamed key reached the assembler %d times", got)
	}
}

// TestKeyRendering pins the shape the pool's logs and its validation
// agree on.
func TestKeyRendering(t *testing.T) {
	for _, tc := range []struct {
		key   Key
		valid bool
		str   string
	}{
		{Key{"workspace", "/ws/a"}, true, "workspace=/ws/a"},
		{Key{"app", "demo"}, true, "app=demo"},
		{Key{}, false, "key=<none>"},
		{Key{Kind: "app"}, false, "key=<none>"},
	} {
		if got := tc.key.Valid(); got != tc.valid {
			t.Errorf("%+v.Valid() = %v, want %v", tc.key, got, tc.valid)
		}
		if got := tc.key.String(); got != tc.str {
			t.Errorf("%+v.String() = %q, want %q", tc.key, got, tc.str)
		}
	}
}

// TestNewValidatesTheSpec pins what a pool cannot work without: an
// assembly, a teardown, and budgets that are not negative.
func TestNewValidatesTheSpec(t *testing.T) {
	open := func(context.Context, Key) (*fakeMember, error) {
		return newFakeMember(Key{}), nil
	}
	closeFn := func(*fakeMember) {}

	for _, tc := range []struct {
		name string
		spec Spec[fakeMember]
	}{
		{"no Open", Spec[fakeMember]{Close: closeFn}},
		{"no Close", Spec[fakeMember]{Open: open}},
		{
			"negative window",
			Spec[fakeMember]{Open: open, Close: closeFn, RetryWindow: -1},
		},
		{
			"negative attempts",
			Spec[fakeMember]{Open: open, Close: closeFn, RetryAttempts: -1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.spec); err == nil {
				t.Fatal("New accepted a spec the pool cannot work with")
			}
		})
	}
	if _, err := New(Spec[fakeMember]{Open: open, Close: closeFn}); err != nil {
		t.Fatalf("New = %v, want a usable pool", err)
	}
}
