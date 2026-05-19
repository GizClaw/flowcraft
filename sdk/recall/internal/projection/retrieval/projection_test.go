package retrieval

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestProjection_UpsertsReservedMetadata(t *testing.T) {
	idx := retrievalmem.New()
	p, err := New(idx)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := model.Scope{RuntimeID: "rt", UserID: "u1"}
	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := model.TemporalFact{
		ID:         "f1",
		Scope:      scope,
		Kind:       model.KindState,
		Content:    "Alice lives in Paris",
		Subject:    "alice",
		Predicate:  "city",
		Object:     "paris",
		Entities:   []string{"alice"},
		MergeKey:   "state|alice|city",
		Confidence: 0.7,
		ObservedAt: validFrom,
		ValidFrom:  &validFrom,
		Metadata:   map[string]any{"user_key": "user_val"},
	}
	if err := p.Project(context.Background(), []model.TemporalFact{f}); err != nil {
		t.Fatalf("project: %v", err)
	}

	got, ok, err := idx.Get(context.Background(), NamespaceFor(scope), "f1")
	if err != nil || !ok {
		t.Fatalf("expected doc upserted, ok=%v err=%v", ok, err)
	}
	if got.Content != "Alice lives in Paris alice city paris alice" {
		t.Errorf("content = %q", got.Content)
	}
	for key, want := range map[string]any{
		model.MetaFactID:    "f1",
		model.MetaFactKind:  string(model.KindState),
		model.MetaMergeKey:  "state|alice|city",
		model.MetaScopeRT:   "rt",
		model.MetaScopeUser: "u1",
		"user_key":          "user_val",
	} {
		if got.Metadata[key] != want {
			t.Errorf("meta[%q] = %v, want %v", key, got.Metadata[key], want)
		}
	}
	if got.Metadata[model.MetaValidFrom].(int64) != validFrom.UnixMilli() {
		t.Errorf("valid_from metadata not in unix-millis: %v", got.Metadata[model.MetaValidFrom])
	}
}

func TestProjection_SearchContentIncludesEvidenceGrounding(t *testing.T) {
	idx := retrievalmem.New()
	p, err := New(idx)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := model.Scope{RuntimeID: "rt", UserID: "u1"}
	f := model.TemporalFact{
		ID:           "f1",
		Scope:        scope,
		Kind:         model.KindEvent,
		Content:      "Caroline joined a support group",
		MergeKey:     "event|caroline|support",
		ObservedAt:   time.Unix(1, 0),
		EvidenceText: "[9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
		EvidenceRefs: []model.EvidenceRef{{
			ID:   "D1:3",
			Text: "Caroline said the group met downtown on 7 May.",
		}},
	}
	if err := p.Project(context.Background(), []model.TemporalFact{f}); err != nil {
		t.Fatalf("project: %v", err)
	}

	resp, err := idx.Search(context.Background(), NamespaceFor(scope), retrieval.SearchRequest{
		QueryText: "LGBTQ downtown 7 May",
		TopK:      5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "f1" {
		t.Fatalf("evidence grounding should be searchable, hits=%+v", resp.Hits)
	}
}

func TestProjection_UserMetaCannotOverrideReserved(t *testing.T) {
	idx := retrievalmem.New()
	p, _ := New(idx)
	scope := model.Scope{RuntimeID: "rt"}
	f := model.TemporalFact{
		ID:         "f1",
		Scope:      scope,
		Kind:       model.KindNote,
		Content:    "x",
		MergeKey:   "k",
		ObservedAt: time.Unix(1, 0),
		Metadata: map[string]any{
			model.MetaFactID:   "spoof",
			model.MetaMergeKey: "spoof",
		},
	}
	if err := p.Project(context.Background(), []model.TemporalFact{f}); err != nil {
		t.Fatalf("project: %v", err)
	}
	got, _, _ := idx.Get(context.Background(), NamespaceFor(scope), "f1")
	if got.Metadata[model.MetaFactID] != "f1" {
		t.Errorf("user metadata leaked into reserved fact_id: %v", got.Metadata[model.MetaFactID])
	}
	if got.Metadata[model.MetaMergeKey] != "k" {
		t.Errorf("user metadata leaked into reserved merge_key: %v", got.Metadata[model.MetaMergeKey])
	}
}

func TestProjection_ForgetRemovesDoc(t *testing.T) {
	idx := retrievalmem.New()
	p, _ := New(idx)
	scope := model.Scope{RuntimeID: "rt"}
	f := model.TemporalFact{
		ID:         "f1",
		Scope:      scope,
		Kind:       model.KindNote,
		MergeKey:   "k",
		ObservedAt: time.Unix(1, 0),
	}
	_ = p.Project(context.Background(), []model.TemporalFact{f})
	if err := p.Forget(context.Background(), scope, []string{"f1"}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	_, ok, _ := idx.Get(context.Background(), NamespaceFor(scope), "f1")
	if ok {
		t.Error("doc should be removed after Forget")
	}
}

func TestProjection_GroupsByScopeNamespace(t *testing.T) {
	idx := retrievalmem.New()
	p, _ := New(idx)
	scopeA := model.Scope{RuntimeID: "rt", UserID: "u1"}
	scopeB := model.Scope{RuntimeID: "rt", UserID: "u2"}
	mk := func(id string, s model.Scope) model.TemporalFact {
		return model.TemporalFact{ID: id, Scope: s, Kind: model.KindNote, MergeKey: "k", ObservedAt: time.Unix(1, 0)}
	}
	err := p.Project(context.Background(), []model.TemporalFact{mk("a", scopeA), mk("b", scopeB)})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scopeA), "a"); !ok {
		t.Error("a not in scopeA namespace")
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scopeB), "b"); !ok {
		t.Error("b not in scopeB namespace")
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scopeA), "b"); ok {
		t.Error("b leaked into scopeA namespace")
	}
}

func TestProjection_RebuildDropsStaleDocs(t *testing.T) {
	idx := retrievalmem.New()
	p, _ := New(idx)
	scope := model.Scope{RuntimeID: "rt", UserID: "u1"}
	fresh := model.TemporalFact{
		ID:         "fresh",
		Scope:      scope,
		Kind:       model.KindNote,
		Content:    "fresh",
		MergeKey:   "note|fresh",
		ObservedAt: time.Unix(1, 0),
	}
	stale := model.TemporalFact{
		ID:         "stale",
		Scope:      scope,
		Kind:       model.KindNote,
		Content:    "stale",
		MergeKey:   "note|stale",
		ObservedAt: time.Unix(1, 0),
	}
	if err := p.Project(context.Background(), []model.TemporalFact{fresh, stale}); err != nil {
		t.Fatalf("initial project: %v", err)
	}
	if err := p.Rebuild(context.Background(), scope, []model.TemporalFact{fresh}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scope), "fresh"); !ok {
		t.Fatal("fresh doc missing after rebuild")
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scope), "stale"); ok {
		t.Fatal("rebuild must remove docs not present in the supplied ledger snapshot")
	}
}

// compile-time guard: retrieval.Index has not regressed in shape.
var _ retrieval.Index = (*retrievalmem.Index)(nil)
