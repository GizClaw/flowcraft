package materialize

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

func TestMaterialize_AttachesFactAndDropsStale(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	scope := model.Scope{RuntimeID: "rt"}
	fact := model.TemporalFact{
		ID:         "real",
		Scope:      scope,
		Kind:       model.KindNote,
		Content:    "hello",
		ObservedAt: time.Unix(1, 0),
		EvidenceRefs: []model.EvidenceRef{
			{ID: "ev1", Text: "evidence"},
		},
	}
	if err := store.Append(context.Background(), []model.TemporalFact{fact}); err != nil {
		t.Fatal(err)
	}
	mat := New(store)
	items, drops, err := mat.Materialize(context.Background(), []model.Candidate{
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
	if len(drops) != 1 || drops[0].Reason != model.DropStaleFact || drops[0].FactID != "ghost" {
		t.Errorf("stale drop = %+v", drops)
	}
}

func TestMaterialize_DropsSuperseded(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	scope := model.Scope{RuntimeID: "rt"}
	original := model.TemporalFact{
		ID: "old", Scope: scope, Kind: model.KindState,
		Content: "old", ObservedAt: time.Unix(1, 0),
	}
	revision := model.TemporalFact{
		ID: "new", Scope: scope, Kind: model.KindState,
		Content: "new", ObservedAt: time.Unix(2, 0),
	}
	if err := store.Append(context.Background(), []model.TemporalFact{original, revision}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateValidity(context.Background(), scope, "old", time.Unix(2, 0), "new"); err != nil {
		t.Fatal(err)
	}
	mat := New(store)
	items, drops, err := mat.Materialize(context.Background(), []model.Candidate{
		{FactID: "old", Scope: scope, Source: "retrieval"},
		{FactID: "new", Scope: scope, Source: "retrieval"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Fact.ID != "new" {
		t.Errorf("expected only the active revision, got %+v", items)
	}
	if len(drops) != 1 || drops[0].Reason != model.DropSuperseded {
		t.Errorf("superseded drop = %+v", drops)
	}
}
