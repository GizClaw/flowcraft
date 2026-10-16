package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

const forgetSettings = `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "fact": {"strategy": "none"},
  "summary": {"disabled": true},
  "interval": "0"
}`

// servedFactIDs runs one context request and returns the derived facts it
// serves, which is the read a forget has to change and a restore has to undo.
func servedFactIDs(t *testing.T, assembly *Assembly, query string) []string {
	t.Helper()
	result, err := assembly.Context(context.Background(), corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: query,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		if item.Kind == corememory.ContextFact {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// TestSoftForgetKeepsADerivedFactOutOfTheReadPath is the O9 acceptance at the
// assembly surface: a forgotten fact is not served, it stays in the view and in
// the ledger while it is gone, restoring it brings it back, and a re-derivation
// under a later policy does not resurrect it -- the fact id is derived from the
// fact's content, so the new generation derives the identity that is already
// forgotten.
func TestSoftForgetKeepsADerivedFactOutOfTheReadPath(t *testing.T) {
	ctx := context.Background()
	assembly, ws := newTestAssemblyWith(t, forgetSettings, WithDeriver(perInputDeriver{}))
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	facts, err := assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts = %#v, %v", facts, err)
	}
	fact := facts[0]
	if ids := servedFactIDs(t, assembly, "beverages"); len(ids) != 1 || ids[0] != fact.ID {
		t.Fatalf("served facts before the forget = %#v", ids)
	}

	address := corememory.ContextAddress{Kind: corememory.ContextFact, ConversationID: "conv-1", ItemID: fact.ID}
	records, err := assembly.SoftForget(ctx, testScope(), []corememory.ContextAddress{address}, "ticket-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Address != address || records[0].Reason != "ticket-42" {
		t.Fatalf("records = %#v", records)
	}

	// The read path drops it; nothing else does. The fact is still in the
	// view -- a forget is unreachability, not removal -- the ledger names it,
	// and the recent lane still serves the turn the fact came from, because
	// the canonical transcript is never filtered.
	if ids := servedFactIDs(t, assembly, "beverages"); len(ids) != 0 {
		t.Fatalf("served facts after the forget = %#v", ids)
	}
	stored, found, err := assembly.Facts().Get(ctx, testScope(), "conv-1", fact.ID)
	if err != nil || !found || stored.ID != fact.ID {
		t.Fatalf("stored fact = %#v, %v, %v", stored, found, err)
	}
	ledger, err := assembly.Forgotten(ctx, testScope(), "conv-1")
	if err != nil || len(ledger) != 1 || ledger[0].Identity != records[0].Identity {
		t.Fatalf("ledger = %#v, %v", ledger, err)
	}
	diagnostics, err := assembly.Diagnostics(ctx)
	if err != nil || len(diagnostics.Scopes) != 1 || diagnostics.Scopes[0].Hidden == nil ||
		diagnostics.Scopes[0].Hidden.Items != 1 {
		t.Fatalf("diagnostics = %#v, %v", diagnostics, err)
	}
	if !servedRecentText(t, assembly, "beverages") {
		t.Fatal("the canonical turn left the recent lane with the fact")
	}

	// A re-derivation under a later policy publishes a new generation of the
	// same conversation. The fact id is content-addressed, so the generation
	// derives the identity the ledger already covers -- and the read path has
	// to keep dropping it, or a policy change would resurrect what a host was
	// told was unreachable.
	second := newTestAssemblyOn(t, ws, forgetSettings, WithDeriver(perInputDeriver{}), WithDeriverVersion("policy-b"))
	t.Cleanup(func() { _ = second.Close() })
	if err := second.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rederived, err := second.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil || len(rederived) != 1 {
		t.Fatalf("re-derived facts = %#v, %v", rederived, err)
	}
	if rederived[0].ID != fact.ID || rederived[0].Generation == fact.Generation {
		t.Fatalf("re-derived fact = %#v, want %q of another generation than %q",
			rederived[0], fact.ID, fact.Generation)
	}
	if ids := servedFactIDs(t, second, "beverages"); len(ids) != 0 {
		t.Fatalf("served facts after the re-derivation = %#v", ids)
	}

	// Restoring the record is the whole undo: the item the generation derives
	// is served again, and the ledger is empty.
	removed, err := second.RestoreForgotten(ctx, testScope(), []corememory.ContextAddress{address})
	if err != nil || len(removed) != 1 || removed[0].Identity != records[0].Identity {
		t.Fatalf("removed = %#v, %v", removed, err)
	}
	if ids := servedFactIDs(t, second, "beverages"); len(ids) != 1 || ids[0] != fact.ID {
		t.Fatalf("served facts after the restore = %#v", ids)
	}
	if ledger, err := second.Forgotten(ctx, testScope(), ""); err != nil || len(ledger) != 0 {
		t.Fatalf("ledger after the restore = %#v, %v", ledger, err)
	}
}

// TestSoftForgetKeepsADerivedSummaryOutOfTheReadPath pins the address of a
// record a fact was compressed into. Hiding is per address: a summary that
// quotes a forgotten fact is its own item and stays servable until the host
// forgets it too, and hiding it leaves the records and the facts it was built
// from where they are.
func TestSoftForgetKeepsADerivedSummaryOutOfTheReadPath(t *testing.T) {
	ctx := context.Background()
	assembly, _ := newTestAssemblyWith(t, generationSettings, WithDeriver(perInputDeriver{}))
	for index, text := range []string{"we talked about beverages", "and about tea"} {
		turn := corememory.Turn{
			Scope: testScope(), ConversationID: "conv-1",
			IdempotencyKey: fmt.Sprintf("run-%d", index+1),
			Messages:       []coremessage.Message{textMessage(coremessage.RoleUser, text)},
		}
		if err := assembly.CommitTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
		if err := assembly.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	served, err := countSummaryItems(assembly)
	if err != nil {
		t.Fatal(err)
	}
	if served == 0 {
		t.Fatal("the fixture serves no summaries before the forget")
	}
	manifest, records, found, err := assembly.Summaries().ActiveSnapshot(ctx, testScope(), "conv-1")
	if err != nil || !found || len(records) == 0 {
		t.Fatalf("summary snapshot = %#v, %#v, %v, %v", manifest, records, found, err)
	}
	addresses := make([]corememory.ContextAddress, 0, len(manifest.RecordIDs))
	for _, record := range records {
		addresses = append(addresses, corememory.ContextAddress{
			Kind: corememory.ContextSummary, ConversationID: record.ConversationID, ItemID: record.ID,
		})
	}
	if _, err := assembly.SoftForget(ctx, testScope(), addresses, "ticket-42"); err != nil {
		t.Fatal(err)
	}

	if served, err = countSummaryItems(assembly); err != nil || served != 0 {
		t.Fatalf("served summaries after the forget = %d, %v", served, err)
	}
	// The facts the summaries were built from are a different address: they
	// are served, and the records are still in the view.
	if count, err := countItems(assembly, corememory.ContextSourceLongTerm); err != nil || count == 0 {
		t.Fatalf("served facts after the forget = %d, %v", count, err)
	}
	if _, again, found, err := assembly.Summaries().ActiveSnapshot(ctx, testScope(), "conv-1"); err != nil || !found || len(again) != len(records) {
		t.Fatalf("summary snapshot after the forget = %#v, %v, %v", again, found, err)
	}

	if _, err := assembly.RestoreForgotten(ctx, testScope(), addresses); err != nil {
		t.Fatal(err)
	}
	if served, err = countSummaryItems(assembly); err != nil || served == 0 {
		t.Fatalf("served summaries after the restore = %d, %v", served, err)
	}
}

// servedRecentText reports whether the recent lane serves one conversation's
// own message text, which is what a forget must not touch.
func servedRecentText(t *testing.T, assembly *Assembly, text string) bool {
	t.Helper()
	result, err := assembly.Context(context.Background(), corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: text,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Items {
		if item.SourceClass == corememory.ContextSourceRecent && strings.Contains(item.Content.Text(), text) {
			return true
		}
	}
	return false
}
