package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestRecall_FederationUserPlusGlobal(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	ctx := context.Background()
	userScope := Scope{RuntimeID: "rt", UserID: "alice"}
	globalScope := Scope{RuntimeID: "rt"}

	if _, err := mem.Save(ctx, userScope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alice loves espresso coffee", Subject: "alice", Predicate: "coffee"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Save(ctx, globalScope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "the office stocks fairtrade coffee", Subject: "office", Predicate: "coffee"}},
	}); err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, userScope)
	drainSideEffectsForTest(t, mem, globalScope)

	defaultHits, err := mem.Recall(ctx, userScope, Query{Text: "coffee", Predicate: "coffee", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var seenAlice, seenOffice bool
	for _, h := range defaultHits {
		if h.Fact.Subject == "alice" {
			seenAlice = true
		}
		if h.Fact.Subject == "office" {
			seenOffice = true
		}
	}
	if !seenAlice || seenOffice {
		t.Fatalf("default recall: alice=%v office=%v hits=%d", seenAlice, seenOffice, len(defaultHits))
	}

	fedScope := userScope
	fedScope.Federation = []Scope{{RuntimeID: "rt"}}
	multiHits, err := mem.Recall(ctx, fedScope, Query{Text: "coffee", Predicate: "coffee", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	seenAlice, seenOffice = false, false
	for _, h := range multiHits {
		if h.Fact.Subject == "alice" {
			seenAlice = true
		}
		if h.Fact.Subject == "office" {
			seenOffice = true
		}
	}
	if !seenAlice || !seenOffice {
		t.Fatalf("federation recall: alice=%v office=%v hits=%d", seenAlice, seenOffice, len(multiHits))
	}
}

func TestForgetSoft_HiddenUntilIncludeRetired(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "retire me"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.FactIDs[0]
	drainSideEffectsForTest(t, mem, scope)
	if err := mem.Forget(ctx, scope, id, ForgetSoft); err != nil {
		t.Fatal(err)
	}
	hits, err := mem.Recall(ctx, scope, Query{Text: "retire", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("soft forget should hide from recall, got %d hits", len(hits))
	}
	// Retrieval projection evicts soft-closed facts on reproject, so
	// BM25 recall cannot resurrect them even with IncludeRetired.
	// Canonical History below still reads the ledger directly.
	withRetired, err := mem.Recall(ctx, scope, Query{Text: "retire", Limit: 5, IncludeRetired: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withRetired) != 0 {
		t.Fatalf("soft-forgotten fact must not remain in retrieval index, got %d hits", len(withRetired))
	}
	versions, err := mem.History(ctx, scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("History should still see soft-forgotten fact")
	}
}

func TestHistory_SupersedeChain(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactState, Subject: "a", Predicate: "p", Object: "one", Content: "one"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactState, Subject: "a", Predicate: "p", Object: "two", Content: "two"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	versions, err := mem.History(ctx, scope, second.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) < 2 {
		t.Fatalf("history len = %d, want >= 2", len(versions))
	}
	_ = first
	_ = domain.KindState
}
