package forget

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

var testNow = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

func testStore(t *testing.T) (*Store, storage.Store) {
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
	store, err := NewStore(kv, WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatal(err)
	}
	return store, kv
}

func testScope() corememory.Scope {
	return corememory.Scope{RuntimeID: "runtime", UserID: "user"}
}

func factAddress(conversationID, factID string) corememory.ContextAddress {
	return corememory.ContextAddress{Kind: corememory.ContextFact, ConversationID: conversationID, ItemID: factID}
}

// listOnlyKV hides the optional key-only scan: the shape a substrate without
// one has, which is the branch the ledger falls back to when it reads keys.
type listOnlyKV struct{ storage.Store }

// TestForgetRecordsTheIdentityTheReadPathLooksUp pins what one forget writes
// and what every read of it returns: the record is addressed by the same
// identity the provider computes for the item, the ledger reads back with the
// reason it was given, and the overlay reads agree with each other.
func TestForgetRecordsTheIdentityTheReadPathLooksUp(t *testing.T) {
	ctx := context.Background()
	store, kv := testStore(t)
	scope := testScope()
	address := factAddress("conv-1", "fact-alice-tea")
	identity := address.Identity(scope)

	records, err := store.Forget(ctx, scope, []corememory.ContextAddress{address}, "ticket-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Identity != identity || records[0].Reason != "ticket-42" ||
		!records[0].HiddenAt.Equal(testNow) || records[0].Address != address {
		t.Fatalf("records = %#v", records)
	}
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := kv.Get(ctx, rootPrefix+"/"+partition+"/items/"+identity)
	if err != nil || len(stored) == 0 {
		t.Fatalf("stored record = %d bytes, %v", len(stored), err)
	}

	visible, err := store.Visible(ctx, scope, identity)
	if err != nil || visible {
		t.Fatalf("visible = %v, %v", visible, err)
	}
	overlay, err := store.SoftForgotten(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, forgotten := overlay[identity]; !forgotten || len(overlay) != 1 {
		t.Fatalf("overlay = %#v", overlay)
	}
	ledger, err := store.Forgotten(ctx, scope, "conv-1")
	if err != nil || len(ledger) != 1 || ledger[0].Identity != identity {
		t.Fatalf("ledger = %#v, %v", ledger, err)
	}
	diagnostics, err := store.Diagnostics(ctx, scope)
	if err != nil || diagnostics.Items != 1 {
		t.Fatalf("diagnostics = %#v, %v", diagnostics, err)
	}

	// An identity the ledger does not cover is one point lookup that misses.
	other, err := store.Visible(ctx, scope, factAddress("conv-1", "fact-other").Identity(scope))
	if err != nil || !other {
		t.Fatalf("unforgotten visible = %v, %v", other, err)
	}
}

// TestForgetKeepsTheFirstRecord pins the ledger's answer to a repeated request:
// the item has been forgotten since the first one, and a retry writes nothing.
func TestForgetKeepsTheFirstRecord(t *testing.T) {
	ctx := context.Background()
	store, kv := testStore(t)
	scope := testScope()
	address := factAddress("conv-1", "fact-alice-tea")

	if _, err := store.Forget(ctx, scope, []corememory.ContextAddress{address}, "first"); err != nil {
		t.Fatal(err)
	}
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		t.Fatal(err)
	}
	key := rootPrefix + "/" + partition + "/items/" + address.Identity(scope)
	before, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	// A second writer with a different clock and reason: the stored record has
	// to survive both, and the call has to report the record that stands.
	store.clock = func() time.Time { return testNow.Add(48 * time.Hour) }
	records, err := store.Forget(ctx, scope, []corememory.ContextAddress{address}, "second")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Reason != "first" || !records[0].HiddenAt.Equal(testNow) {
		t.Fatalf("second forget = %#v", records)
	}
	after, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("a repeated forget rewrote the record:\n%s\n%s", before, after)
	}
	// The same address named twice in one call is one record.
	records, err = store.Forget(ctx, scope, []corememory.ContextAddress{address, address}, "third")
	if err != nil || len(records) != 1 {
		t.Fatalf("duplicated forget = %#v, %v", records, err)
	}
}

// TestRestoreReturnsWhatItRemoved pins the reversible half: restoring deletes
// the record it was asked about, reports it, and leaves an item nobody forgot
// alone rather than failing the call.
func TestRestoreReturnsWhatItRemoved(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := testScope()
	hidden := factAddress("conv-1", "fact-alice-tea")
	untouched := factAddress("conv-1", "fact-alice-coffee")
	if _, err := store.Forget(ctx, scope, []corememory.ContextAddress{hidden, untouched}, "ticket-42"); err != nil {
		t.Fatal(err)
	}

	removed, err := store.Restore(ctx, scope, []corememory.ContextAddress{hidden, factAddress("conv-1", "fact-never")})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Identity != hidden.Identity(scope) || removed[0].Reason != "ticket-42" {
		t.Fatalf("removed = %#v", removed)
	}
	if visible, err := store.Visible(ctx, scope, hidden.Identity(scope)); err != nil || !visible {
		t.Fatalf("restored visible = %v, %v", visible, err)
	}
	if visible, err := store.Visible(ctx, scope, untouched.Identity(scope)); err != nil || visible {
		t.Fatalf("untouched visible = %v, %v", visible, err)
	}
	// A second restore is the same request: nothing left to remove.
	again, err := store.Restore(ctx, scope, []corememory.ContextAddress{hidden})
	if err != nil || len(again) != 0 {
		t.Fatalf("second restore = %#v, %v", again, err)
	}
	diagnostics, err := store.Diagnostics(ctx, scope)
	if err != nil || diagnostics.Items != 1 {
		t.Fatalf("diagnostics = %#v, %v", diagnostics, err)
	}
	// The item can be forgotten again, under the decision that follows: the
	// ledger reads as the history it is, oldest first.
	store.clock = func() time.Time { return testNow.Add(24 * time.Hour) }
	if _, err := store.Forget(ctx, scope, []corememory.ContextAddress{hidden}, "ticket-43"); err != nil {
		t.Fatal(err)
	}
	ledger, err := store.Forgotten(ctx, scope, "")
	if err != nil || len(ledger) != 2 {
		t.Fatalf("ledger = %#v, %v", ledger, err)
	}
	if ledger[0].Reason != "ticket-42" || ledger[1].Reason != "ticket-43" {
		t.Fatalf("ledger order = %#v", ledger)
	}
}

// TestLedgerSeparatesScopesAndNarrowsByConversation pins the reads a host makes
// about one tenant and one conversation: a hard scope's ledger says nothing
// about another's, and the conversation read returns that conversation's items,
// including the ones a document address names.
func TestLedgerSeparatesScopesAndNarrowsByConversation(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := testScope()
	other := corememory.Scope{RuntimeID: "runtime", UserID: "other"}
	first := factAddress("conv-1", "fact-a")
	second := factAddress("conv-2", "fact-b")
	document := corememory.ContextAddress{
		Kind: corememory.ContextDocumentChunk, DatasetID: "dataset", DocumentID: "document", ItemID: "chunk-1",
	}
	if _, err := store.Forget(ctx, scope, []corememory.ContextAddress{first, document}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Forget(ctx, other, []corememory.ContextAddress{first}, ""); err != nil {
		t.Fatal(err)
	}

	// Every scope computes its own identity, so one scope's record never
	// answers for another's request.
	if visible, err := store.Visible(ctx, scope, first.Identity(scope)); err != nil || visible {
		t.Fatalf("first scope visible = %v, %v", visible, err)
	}
	if visible, err := store.Visible(ctx, other, first.Identity(other)); err != nil || visible {
		t.Fatalf("other scope visible = %v, %v", visible, err)
	}
	overlay, err := store.SoftForgotten(ctx, scope)
	if err != nil || len(overlay) != 2 {
		t.Fatalf("overlay = %#v, %v", overlay, err)
	}
	if _, crossScope := overlay[first.Identity(other)]; crossScope {
		t.Fatalf("overlay carries another scope's identity: %#v", overlay)
	}

	ledger, err := store.Forgotten(ctx, scope, "conv-1")
	if err != nil || len(ledger) != 1 || ledger[0].Address != first {
		t.Fatalf("conv-1 ledger = %#v, %v", ledger, err)
	}
	if ledger, err := store.Forgotten(ctx, scope, "conv-9"); err != nil || len(ledger) != 0 {
		t.Fatalf("conv-9 ledger = %#v, %v", ledger, err)
	}
	// A document address names no conversation: it is in the whole-scope read,
	// and in no conversation's.
	ledger, err = store.Forgotten(ctx, scope, "")
	if err != nil || len(ledger) != 2 {
		t.Fatalf("scope ledger = %#v, %v", ledger, err)
	}

	if _, err := store.Forget(ctx, scope, []corememory.ContextAddress{second}, ""); err != nil {
		t.Fatal(err)
	}
	ledger, err = store.Forgotten(ctx, scope, "conv-2")
	if err != nil || len(ledger) != 1 || ledger[0].Address != second {
		t.Fatalf("conv-2 ledger = %#v, %v", ledger, err)
	}
}

// TestForgetRefusesAddressesNoReadWouldLookUp pins the writes that must not
// land: a forget nobody can honor is worse than an error, because the ledger
// would report an item as unreachable while the read path kept serving it.
func TestForgetRefusesAddressesNoReadWouldLookUp(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := testScope()
	cases := []struct {
		name string
		// reasonOnly marks a case the reason alone makes invalid: Restore
		// takes no reason, so it has nothing to refuse.
		reasonOnly bool
		scope      corememory.Scope
		address    corememory.ContextAddress
		reason     string
	}{
		{name: "no address", scope: scope, address: corememory.ContextAddress{}},
		{name: "no item id", scope: scope, address: corememory.ContextAddress{Kind: corememory.ContextFact, ConversationID: "conv-1"}},
		{name: "no conversation", scope: scope, address: corememory.ContextAddress{Kind: corememory.ContextFact, ItemID: "fact-a"}},
		{
			name: "recent lane", scope: scope,
			address: corememory.ContextAddress{Kind: corememory.ContextRawMessage, ConversationID: "conv-1", ItemID: "message-1"},
		},
		{
			name: "unknown kind", scope: scope,
			address: corememory.ContextAddress{Kind: "not-a-kind", ConversationID: "conv-1", ItemID: "item"},
		},
		{name: "no runtime id", scope: corememory.Scope{}, address: factAddress("conv-1", "fact-a")},
		{
			name: "untrimmed reason", reasonOnly: true, scope: scope,
			address: factAddress("conv-1", "fact-a"), reason: " padded",
		},
		{
			name: "long reason", reasonOnly: true, scope: scope,
			address: factAddress("conv-1", "fact-a"), reason: string(make([]byte, maxReasonBytes+1)),
		},
	}
	for _, testCase := range cases {
		if _, err := store.Forget(ctx, testCase.scope, []corememory.ContextAddress{testCase.address}, testCase.reason); err == nil {
			t.Fatalf("%s: forget was accepted", testCase.name)
		}
		if testCase.reasonOnly {
			continue
		}
		if _, err := store.Restore(ctx, testCase.scope, []corememory.ContextAddress{testCase.address}); err == nil {
			t.Fatalf("%s: restore was accepted", testCase.name)
		}
	}
	diagnostics, err := store.Diagnostics(ctx, scope)
	if err != nil || diagnostics.Items != 0 {
		t.Fatalf("diagnostics after refused writes = %#v, %v", diagnostics, err)
	}
	// One address the ledger cannot address fails the whole call: the valid
	// address before it is not recorded, so a caller that passed a mixed list
	// has nothing to reconcile.
	if _, err := store.Forget(ctx, scope, []corememory.ContextAddress{
		factAddress("conv-1", "fact-a"), {Kind: corememory.ContextRawMessage, ConversationID: "conv-1", ItemID: "message-1"},
	}, ""); err == nil {
		t.Fatal("a mixed forget was accepted")
	}
	if diagnostics, err = store.Diagnostics(ctx, scope); err != nil || diagnostics.Items != 0 {
		t.Fatalf("diagnostics after a mixed forget = %#v, %v", diagnostics, err)
	}
}

// TestLedgerReadsFailClosed pins what a record nobody can decode does: the item
// stays unservable -- a ledger entry is a statement, and an unreadable one is
// not an absent one -- the lenient read still hides it (the identity is the
// key), and the reads a host audits with report the corruption instead of
// skipping it.
func TestLedgerReadsFailClosed(t *testing.T) {
	ctx := context.Background()
	store, kv := testStore(t)
	scope := testScope()
	address := factAddress("conv-1", "fact-alice-tea")
	identity := address.Identity(scope)
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		t.Fatal(err)
	}
	key := rootPrefix + "/" + partition + "/items/" + identity

	if err := kv.Put(ctx, key, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if visible, err := store.Visible(ctx, scope, identity); err == nil || visible {
		t.Fatalf("corrupt record visible = %v, %v", visible, err)
	}
	if _, err := store.Forgotten(ctx, scope, ""); err == nil {
		t.Fatal("the ledger listed a record it cannot decode")
	}
	overlay, err := store.SoftForgotten(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, forgotten := overlay[identity]; !forgotten {
		t.Fatalf("the bulk overlay dropped an unreadable record: %#v", overlay)
	}

	// A record whose identity is not the one its key names is corruption too:
	// the item stored under the key is not the item its address would hide.
	other := factAddress("conv-1", "fact-other")
	data, err := json.Marshal(Record{Identity: other.Identity(scope), Address: other, HiddenAt: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Forgotten(ctx, scope, ""); err == nil {
		t.Fatal("the ledger listed a record that names another identity")
	}
	if visible, err := store.Visible(ctx, scope, identity); err == nil || visible {
		t.Fatalf("misaddressed record visible = %v, %v", visible, err)
	}
	// SoftForgotten reads keys alone, so the read path keeps hiding the item
	// the key names even while the record under it is unusable.
	overlay, err = store.SoftForgotten(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, forgotten := overlay[identity]; !forgotten {
		t.Fatalf("the bulk overlay dropped a misaddressed record: %#v", overlay)
	}
}

// TestBulkReadFallsBackToList pins the substrate without a key-only scan: the
// overlay is the same one, read through the values the store has to hand back.
func TestBulkReadFallsBackToList(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	inner, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(listOnlyKV{Store: inner})
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	addresses := []corememory.ContextAddress{factAddress("conv-1", "fact-a"), factAddress("conv-1", "fact-b")}
	if _, err := store.Forget(ctx, scope, addresses, "ticket-42"); err != nil {
		t.Fatal(err)
	}
	overlay, err := store.SoftForgotten(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay) != 2 {
		t.Fatalf("overlay = %#v", overlay)
	}
	for _, address := range addresses {
		if _, forgotten := overlay[address.Identity(scope)]; !forgotten {
			t.Fatalf("address %q is missing from %#v", address.ItemID, overlay)
		}
	}
	diagnostics, err := store.Diagnostics(ctx, scope)
	if err != nil || diagnostics.Items != 2 {
		t.Fatalf("diagnostics = %#v, %v", diagnostics, err)
	}
	// An empty scope answers with an empty overlay, not nil: the provider
	// reads nil as "the bulk read failed" and would go back per item.
	empty, err := store.SoftForgotten(ctx, corememory.Scope{RuntimeID: "runtime", UserID: "elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty overlay = %#v, %v", empty, err)
	}
}

// TestLedgerRefusesReadsWithoutAContext pins the one input every method
// validates the same way, and the nil store a caller that built no ledger
// holds: both answer with an error rather than a decision about items.
func TestLedgerRefusesReadsWithoutAContext(t *testing.T) {
	store, _ := testStore(t)
	scope := testScope()
	address := factAddress("conv-1", "fact-a")
	//nolint:staticcheck // deliberate: a call without a context must be refused
	if _, err := store.Restore(nil, scope, []corememory.ContextAddress{address}); err == nil {
		t.Fatal("a restore without a context was accepted")
	}
	//nolint:staticcheck // deliberate: a call without a context must be refused
	if _, err := store.Forget(nil, scope, []corememory.ContextAddress{address}, ""); err == nil {
		t.Fatal("a forget without a context was accepted")
	}
	//nolint:staticcheck // deliberate: a call without a context must be refused
	if _, err := store.Forgotten(nil, scope, ""); err == nil {
		t.Fatal("a ledger read without a context was accepted")
	}
	//nolint:staticcheck // deliberate: a call without a context must be refused
	if _, err := store.Diagnostics(nil, scope); err == nil {
		t.Fatal("a diagnostics read without a context was accepted")
	}
	var nilStore *Store
	//nolint:staticcheck // deliberate: a store that knows no context reads nothing
	if _, err := nilStore.Visible(nil, scope, "context-item-x"); err == nil {
		t.Fatal("a nil store answered a visibility read")
	}
	//nolint:staticcheck // deliberate: a store that knows no context reads nothing
	if _, err := nilStore.SoftForgotten(nil, scope); err == nil {
		t.Fatal("a nil store answered a bulk read")
	}
}
