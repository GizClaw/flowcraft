package materialize

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

func TestMaterialize_AttachesFactAndDropsStale(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	scope := domain.Scope{RuntimeID: "rt"}
	fact := domain.TemporalFact{
		ID:         "real",
		Scope:      scope,
		Kind:       domain.KindNote,
		Content:    "hello",
		ObservedAt: time.Unix(1, 0),
		EvidenceRefs: []domain.EvidenceRef{
			{ID: "ev1", Text: "evidence"},
		},
	}
	if err := store.Append(context.Background(), []domain.TemporalFact{fact}); err != nil {
		t.Fatal(err)
	}
	mat := New(store, nil)
	items, drops, err := mat.Materialize(context.Background(), []domain.Candidate{
		{FactID: "real", Scope: scope, Source: "retrieval", Score: 0.9},
		{FactID: "ghost", Scope: scope, Source: "retrieval", Score: 0.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Fact.ID != "real" {
		t.Fatalf("materialized items = %+v", items)
	}
	if len(items[0].Evidence) != 1 {
		t.Errorf("evidence not attached: %+v", items[0].Evidence)
	}
	if len(drops) != 1 || drops[0].Reason != diagnostic.DropStaleFact || drops[0].FactID != "ghost" {
		t.Errorf("stale drop = %+v", drops)
	}
}

func TestMaterialize_SelectsCandidateEvidence(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	scope := domain.Scope{RuntimeID: "rt"}
	fact := domain.TemporalFact{
		ID:      "real",
		Scope:   scope,
		Kind:    domain.KindNote,
		Content: "hello",
		EvidenceRefs: []domain.EvidenceRef{
			{ID: "ev1", Text: "first evidence"},
			{ID: "ev2", MessageID: "msg-2", Text: "second evidence"},
		},
	}
	if err := store.Append(context.Background(), []domain.TemporalFact{fact}); err != nil {
		t.Fatal(err)
	}
	mat := New(store, nil)
	items, _, err := mat.Materialize(context.Background(), []domain.Candidate{
		{FactID: "real", Scope: scope, Source: "retrieval", EvidenceIDs: []string{"msg-2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("materialized items = %+v", items)
	}
	if len(items[0].Evidence) != 1 || items[0].Evidence[0].ID != "ev2" {
		t.Fatalf("selected evidence = %+v", items[0].Evidence)
	}
}

func TestMaterialize_DropsSuperseded(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	scope := domain.Scope{RuntimeID: "rt"}
	original := domain.TemporalFact{
		ID: "old", Scope: scope, Kind: domain.KindState,
		Content: "old", ObservedAt: time.Unix(1, 0),
	}
	revision := domain.TemporalFact{
		ID: "new", Scope: scope, Kind: domain.KindState,
		Content: "new", ObservedAt: time.Unix(2, 0),
	}
	if err := store.Append(context.Background(), []domain.TemporalFact{original, revision}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateValidity(context.Background(), scope, "old", time.Unix(2, 0), "new"); err != nil {
		t.Fatal(err)
	}
	mat := New(store, nil)
	items, drops, err := mat.Materialize(context.Background(), []domain.Candidate{
		{FactID: "old", Scope: scope, Source: "retrieval"},
		{FactID: "new", Scope: scope, Source: "retrieval"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Fact.ID != "new" {
		t.Errorf("expected only the active revision, got %+v", items)
	}
	if len(drops) != 1 || drops[0].Reason != diagnostic.DropSuperseded {
		t.Errorf("superseded drop = %+v", drops)
	}
}
