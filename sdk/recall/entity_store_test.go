package recall_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	memidx "github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

// fixedClock returns a deterministic time source so we can assert
// MetaEntityLast moves between writes without depending on wall
// time.
func fixedClock(t *testing.T) (advance func(d time.Duration), now func() time.Time) {
	t.Helper()
	current := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	return func(d time.Duration) {
			current = current.Add(d)
		}, func() time.Time {
			return current
		}
}

func newEntityStore(t *testing.T, capN int) (*recall.IndexEntityStore, func(time.Duration)) {
	t.Helper()
	idx := memidx.New()
	advance, now := fixedClock(t)
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{
		LinkedCap: capN,
		Clock:     now,
	})
	if store == nil {
		t.Fatalf("NewIndexEntityStore returned nil; memidx.Index does not satisfy DocGetter?")
	}
	return store, advance
}

// TestEntityKeyComposite pins the row ID encoding so backends that
// log writes can rely on the format.
func TestEntityKeyComposite(t *testing.T) {
	// UserID is fed through saneNS, so the raw test inputs are
	// pre-sanitised to match what the row key will actually be. This
	// keeps the encoding contract symmetric with NamespaceFor.
	cases := []struct {
		name  string
		scope recall.Scope
		raw   string
		want  string
	}{
		{"user_lowercase", recall.Scope{UserID: "u1"}, "Alice", "u1::alice"},
		{"user_possessive", recall.Scope{UserID: "u1"}, "Alice's", "u1::alice"},
		{"user_trimmed_punct", recall.Scope{UserID: "u1"}, "  Alice.  ", "u1::alice"},
		{"empty_user_is_global", recall.Scope{UserID: ""}, "Bob", "anon::bob"},
		{"conv_isolation_sanitised", recall.Scope{UserID: "conv-26"}, "Alice", "conv_26::alice"},
		{"different_convs_no_collide", recall.Scope{UserID: "conv-30"}, "Alice", "conv_30::alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recall.EntityKey(tc.scope, tc.raw); got != tc.want {
				t.Fatalf("EntityKey(%q, %q) = %q; want %q", tc.scope.UserID, tc.raw, got, tc.want)
			}
		})
	}
}

func TestEntityNamespaceFor(t *testing.T) {
	scope := recall.Scope{RuntimeID: "rt1", UserID: "conv-26"}
	got := recall.EntityNamespaceFor(scope)
	want := recall.NamespaceFor(scope) + "__entities"
	if got != want {
		t.Fatalf("EntityNamespaceFor(%v) = %q; want %q", scope, got, want)
	}
	// Must remain in [A-Za-z0-9_] character set so backend
	// namespace validators accept it.
	for _, r := range got {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
		default:
			t.Fatalf("EntityNamespaceFor returned char %q outside saneNS set: %q", r, got)
		}
	}
}

// TestLinkAndLookupRoundtrip is the happy path: a few entities, a
// few entries each, Lookup returns the deduplicated union in
// deterministic order.
func TestLinkAndLookupRoundtrip(t *testing.T) {
	store, _ := newEntityStore(t, 100)
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()

	err := store.Link(ctx, scope, map[string][]string{
		"alice": {"e1", "e2"},
		"bob":   {"e2", "e3"},
	})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}

	// Query in mixed case to ensure normalize() is applied; ask for
	// both Alice and Bob to verify union + dedup.
	got, err := store.Lookup(ctx, scope, []string{"Alice", "BOB"}, 0)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// Sort what we got for assertion stability — entity order is
	// alpha-sorted by Lookup so we expect e1,e2 (alice) then e3 (bob);
	// e2 deduped because already seen.
	if want := []string{"e1", "e2", "e3"}; !equalSlice(got, want) {
		t.Fatalf("Lookup union got %v; want %v", got, want)
	}
}

// TestLinkIdempotent verifies that linking the same (entity, id)
// twice does not double-count — needed for Save retries.
func TestLinkIdempotent(t *testing.T) {
	store, _ := newEntityStore(t, 100)
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	if err := store.Link(ctx, scope, map[string][]string{"alice": {"e1", "e2"}}); err != nil {
		t.Fatalf("Link1: %v", err)
	}
	// Retry with overlap + new id.
	if err := store.Link(ctx, scope, map[string][]string{"alice": {"e1", "e2", "e3"}}); err != nil {
		t.Fatalf("Link2: %v", err)
	}
	got, err := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if want := []string{"e1", "e2", "e3"}; !equalSlice(got, want) {
		t.Fatalf("Lookup got %v; want %v", got, want)
	}
}

// TestLinkFIFOCap verifies cap eviction is FIFO and drops the
// oldest id when len would exceed LinkedCap.
func TestLinkFIFOCap(t *testing.T) {
	store, _ := newEntityStore(t, 3)
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	// Three separate Link calls so insertion order is unambiguous.
	for _, id := range []string{"e1", "e2", "e3", "e4", "e5"} {
		if err := store.Link(ctx, scope, map[string][]string{"alice": {id}}); err != nil {
			t.Fatalf("Link %s: %v", id, err)
		}
	}
	got, err := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if want := []string{"e3", "e4", "e5"}; !equalSlice(got, want) {
		t.Fatalf("FIFO cap: got %v; want %v (oldest two should be evicted)", got, want)
	}
}

// TestLookupPerEntityCap verifies the recency-first cap.
func TestLookupPerEntityCap(t *testing.T) {
	store, _ := newEntityStore(t, 100)
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	if err := store.Link(ctx, scope, map[string][]string{
		"alice": {"e1", "e2", "e3", "e4", "e5"},
	}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	got, err := store.Lookup(ctx, scope, []string{"alice"}, 2)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// Cap = 2 should return the LAST two (most recent under FIFO
	// write order).
	if want := []string{"e4", "e5"}; !equalSlice(got, want) {
		t.Fatalf("per-entity cap=2 got %v; want %v", got, want)
	}
}

// TestLookupMissingEntity returns empty without error.
func TestLookupMissingEntity(t *testing.T) {
	store, _ := newEntityStore(t, 100)
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	got, err := store.Lookup(ctx, scope, []string{"nobody"}, 0)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("missing entity should return empty; got %v", got)
	}
}

// TestLookupEmptyInputs short-circuits without backend round-trip.
func TestLookupEmptyInputs(t *testing.T) {
	store, _ := newEntityStore(t, 100)
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	got, err := store.Lookup(ctx, scope, nil, 0)
	if err != nil {
		t.Fatalf("Lookup(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Lookup(nil) want empty; got %v", got)
	}
	got, err = store.Lookup(ctx, scope, []string{""}, 0)
	if err != nil {
		t.Fatalf("Lookup(empty atom): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Lookup(empty atom) want empty; got %v", got)
	}
}

// TestScopeIsolation verifies that the composite key keeps different
// users separated.
func TestScopeIsolation(t *testing.T) {
	store, _ := newEntityStore(t, 100)
	a := recall.Scope{UserID: "conv-26"}
	b := recall.Scope{UserID: "conv-30"}
	ctx := context.Background()
	if err := store.Link(ctx, a, map[string][]string{"alice": {"e-a1", "e-a2"}}); err != nil {
		t.Fatalf("Link a: %v", err)
	}
	if err := store.Link(ctx, b, map[string][]string{"alice": {"e-b1"}}); err != nil {
		t.Fatalf("Link b: %v", err)
	}
	// Different scopes share the same entity NAME but the rows are
	// in different namespaces. Each Lookup must see only its own.
	gotA, _ := store.Lookup(ctx, a, []string{"alice"}, 0)
	gotB, _ := store.Lookup(ctx, b, []string{"alice"}, 0)
	if want := []string{"e-a1", "e-a2"}; !equalSlice(gotA, want) {
		t.Fatalf("scope a: got %v; want %v", gotA, want)
	}
	if want := []string{"e-b1"}; !equalSlice(gotB, want) {
		t.Fatalf("scope b: got %v; want %v", gotB, want)
	}
}

// TestForgetPrunesAllReferences ensures every entity row that
// references the removed entry id is rewritten.
func TestForgetPrunesAllReferences(t *testing.T) {
	store, _ := newEntityStore(t, 100)
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	if err := store.Link(ctx, scope, map[string][]string{
		"alice": {"e1", "e2"},
		"bob":   {"e2", "e3"},
		"carol": {"e4"},
	}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	if err := store.Forget(ctx, scope, "e2"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	// alice should now have only [e1]; bob only [e3]; carol unchanged.
	if got, _ := store.Lookup(ctx, scope, []string{"alice"}, 0); !equalSlice(got, []string{"e1"}) {
		t.Fatalf("alice after Forget(e2): got %v; want [e1]", got)
	}
	if got, _ := store.Lookup(ctx, scope, []string{"bob"}, 0); !equalSlice(got, []string{"e3"}) {
		t.Fatalf("bob after Forget(e2): got %v; want [e3]", got)
	}
	if got, _ := store.Lookup(ctx, scope, []string{"carol"}, 0); !equalSlice(got, []string{"e4"}) {
		t.Fatalf("carol untouched by Forget(e2): got %v; want [e4]", got)
	}
}

// TestForgetNoOpForUnknownID exercises the "id is not referenced"
// path; nothing should change.
func TestForgetNoOpForUnknownID(t *testing.T) {
	store, _ := newEntityStore(t, 100)
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	if err := store.Link(ctx, scope, map[string][]string{"alice": {"e1"}}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if err := store.Forget(ctx, scope, "ghost-id"); err != nil {
		t.Fatalf("Forget ghost: %v", err)
	}
	got, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if want := []string{"e1"}; !equalSlice(got, want) {
		t.Fatalf("after no-op Forget got %v; want %v", got, want)
	}
}

// TestLookupCommonNounGate verifies that MaxLinkedCount silently
// drops entity rows whose linked count exceeds the threshold while
// preserving rows under the threshold. This is the row-8
// pollution-defence: an entity that has linked to "too many"
// entries is treated as a common noun and yields zero candidates
// at Lookup time so RRF cannot rank-vote that low-signal set into
// the fused output.
func TestLookupCommonNounGate(t *testing.T) {
	idx := memidx.New()
	_, now := fixedClock(t)
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{
		LinkedCap:      100,
		MaxLinkedCount: 5,
		Clock:          now,
	})
	if store == nil {
		t.Fatalf("NewIndexEntityStore returned nil")
	}
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()

	// "alice" stays under the gate (3 linked); "the" exceeds it
	// (8 linked).
	if err := store.Link(ctx, scope, map[string][]string{
		"alice": {"e1", "e2", "e3"},
		"the":   {"x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8"},
	}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	gotAlice, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if !equalSlice(gotAlice, []string{"e1", "e2", "e3"}) {
		t.Fatalf("alice (under gate) got %v; want full list", gotAlice)
	}
	gotThe, _ := store.Lookup(ctx, scope, []string{"the"}, 0)
	if len(gotThe) != 0 {
		t.Fatalf("the (over gate) got %v; want empty (common-noun gate should drop)", gotThe)
	}

	// Union query: alice (rare) survives, the (common) is dropped
	// silently — operator does not lose recall just because the
	// query touched a common atom alongside a rare one.
	gotUnion, _ := store.Lookup(ctx, scope, []string{"alice", "the"}, 0)
	if !equalSlice(gotUnion, []string{"e1", "e2", "e3"}) {
		t.Fatalf("union got %v; want alice's list (the should be silently dropped)", gotUnion)
	}
}

// TestLookupCommonNounGate_DefaultIsSafe pins the post-es-default
// semantic flip: MaxLinkedCount==0 now means "apply the safe
// production default" (100), not "no gate". Pre-change 0 was the
// dangerous path for entity-dense conversational corpora; making the
// safe value the no-opinion default prevents accidental footguns.
//
// Asserted indirectly: link 150 ids under a single entity — well
// above the 100 default — and Lookup must return ZERO ids (the
// gate fired). With the pre-change semantic Lookup would have
// returned all 150 verbatim.
func TestLookupCommonNounGate_DefaultIsSafe(t *testing.T) {
	store, _ := newEntityStore(t, 200) // MaxLinkedCount unset → default 100
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	// 150 ids under one entity — saturates the row well past the
	// safe default (100).
	ids := make([]string, 150)
	for i := range ids {
		ids[i] = fmt.Sprintf("x%03d", i)
	}
	if err := store.Link(ctx, scope, map[string][]string{"crowded": ids}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	got, _ := store.Lookup(ctx, scope, []string{"crowded"}, 0)
	if len(got) != 0 {
		t.Fatalf("safe default not applied: got %d ids past gate=100; want 0", len(got))
	}
}

// TestLookupCommonNounGate_ExplicitlyDisabled covers the opt-out
// path documented on [IndexEntityStoreOptions.MaxLinkedCount]: a
// negative value means "I have audited my corpus, turn the gate
// off". The store honours it (Lookup returns the full list),
// distinct from "unset" (default applies).
func TestLookupCommonNounGate_ExplicitlyDisabled(t *testing.T) {
	idx := memidx.New()
	_, now := fixedClock(t)
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{
		LinkedCap:      200,
		MaxLinkedCount: -1, // explicit opt-out
		Clock:          now,
	})
	if store == nil {
		t.Fatalf("NewIndexEntityStore returned nil")
	}
	scope := recall.Scope{UserID: "u1"}
	ctx := context.Background()
	ids := make([]string, 150)
	for i := range ids {
		ids[i] = fmt.Sprintf("x%03d", i)
	}
	if err := store.Link(ctx, scope, map[string][]string{"crowded": ids}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	got, _ := store.Lookup(ctx, scope, []string{"crowded"}, 0)
	if len(got) != 150 {
		t.Fatalf("explicit disable not honoured: got %d ids; want 150 (full list)", len(got))
	}
}

// TestWithEntityStoreMaxLinkedCount_WarnsOnExplicitDisable pins the
// audit trail half of es-default: passing a negative value to
// [WithEntityStoreMaxLinkedCount] is the documented opt-out for
// the common-noun pollution gate, but because that path can be a
// footgun in entity-dense corpora [Memory.New] MUST log a one-time
// warning at construction so the choice is
// not silently lost in deployment.
//
// The complementary assertions:
//
//   - No-opinion default (no option set) → no warning, gate at
//     safe default 100.
//   - Explicit positive override → no warning, that value applies.
func TestWithEntityStoreMaxLinkedCount_WarnsOnExplicitDisable(t *testing.T) {
	t.Run("explicit_disable_warns", func(t *testing.T) {
		var lines []string
		m, err := recall.New(memidx.New(),
			recall.WithRequireUserID(),
			recall.WithEntityStore(0),
			recall.WithEntityStoreMaxLinkedCount(-1),
			recall.WithLogger(func(format string, args ...any) {
				lines = append(lines, fmt.Sprintf(format, args...))
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer m.Close()
		found := false
		for _, l := range lines {
			if containsAll(l, []string{"WithEntityStoreMaxLinkedCount", "explicitly disables"}) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("explicit-disable warning not emitted; logger lines=%v", lines)
		}
	})

	t.Run("default_is_silent", func(t *testing.T) {
		var lines []string
		m, err := recall.New(memidx.New(),
			recall.WithRequireUserID(),
			recall.WithEntityStore(0), // no explicit MaxLinkedCount
			recall.WithLogger(func(format string, args ...any) {
				lines = append(lines, fmt.Sprintf(format, args...))
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer m.Close()
		for _, l := range lines {
			if containsAll(l, []string{"WithEntityStoreMaxLinkedCount", "explicitly disables"}) {
				t.Fatalf("safe default path should not warn; got %q", l)
			}
		}
	})

	t.Run("positive_override_silent", func(t *testing.T) {
		var lines []string
		m, err := recall.New(memidx.New(),
			recall.WithRequireUserID(),
			recall.WithEntityStore(0),
			recall.WithEntityStoreMaxLinkedCount(64),
			recall.WithLogger(func(format string, args ...any) {
				lines = append(lines, fmt.Sprintf(format, args...))
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer m.Close()
		for _, l := range lines {
			if containsAll(l, []string{"WithEntityStoreMaxLinkedCount", "explicitly disables"}) {
				t.Fatalf("positive override should not warn; got %q", l)
			}
		}
	})
}

// containsAll reports whether s contains every substring in subs.
func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		idx := -1
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				idx = i
				break
			}
		}
		if idx < 0 {
			return false
		}
	}
	return true
}

// TestNewIndexEntityStoreNilOnNilIndex ensures we degrade rather
// than panic when the index is nil. The other half of this contract
// (index that does not satisfy DocGetter -> nil store) is exercised
// at the retrieval-package boundary; we deliberately keep this test
// scope narrow so we don't have to define a hand-rolled Index stub
// inside the recall package's external test binary.
func TestNewIndexEntityStoreNilOnNilIndex(t *testing.T) {
	if got := recall.NewIndexEntityStore(nil, recall.IndexEntityStoreOptions{}); got != nil {
		t.Fatalf("nil index should produce nil store; got %T", got)
	}
}

// TestSaveLinksEntitiesIntoStore verifies the upsertFacts → Link
// integration: enabling WithEntityStore should make every fact's
// normalized entities reachable via EntityStore.Lookup keyed on the
// same atoms produced by NormalizeEntities.
func TestSaveLinksEntitiesIntoStore(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[
        {"content":"Alice likes black coffee","entities":["Alice","black coffee"]},
        {"content":"Bob plays tennis","entities":["Bob","tennis"]}
    ]`
	m, err := recall.New(idx,
		recall.WithLLM(&stubLLM{resp: resp}),
		recall.WithEntityStore(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	res, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "I like Alice and Bob"}}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(res.EntryIDs) != 2 {
		t.Fatalf("expected 2 entries; got %v", res.EntryIDs)
	}

	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter on this build")
	}
	gotAlice, err := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if err != nil {
		t.Fatalf("Lookup alice: %v", err)
	}
	if len(gotAlice) == 0 {
		t.Fatalf("entity store has no link for 'alice'; entries=%v", res.EntryIDs)
	}
	gotBob, _ := store.Lookup(ctx, scope, []string{"bob"}, 0)
	if len(gotBob) == 0 {
		t.Fatalf("entity store has no link for 'bob'; entries=%v", res.EntryIDs)
	}

	// Forget should drop the entry id from the inverted index.
	if err := m.Forget(ctx, scope, res.EntryIDs[0], "test"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	post, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	for _, id := range post {
		if id == res.EntryIDs[0] {
			t.Fatalf("Forget did not prune entity link for %q", id)
		}
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
