package materialize

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	linkstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/link"
	observationstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/observation"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
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
	mat := New(store, nil, nil, nil)
	items, drops, err := mat.Materialize(context.Background(), []domain.Candidate{
		{Kind: domain.GraphNodeAssertion, ID: "real", Scope: scope, Source: "retrieval", Score: 0.9},
		{Kind: domain.GraphNodeAssertion, ID: "ghost", Scope: scope, Source: "retrieval", Score: 0.5},
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
	mat := New(store, nil, nil, nil)
	items, _, err := mat.Materialize(context.Background(), []domain.Candidate{
		{Kind: domain.GraphNodeAssertion, ID: "real", Scope: scope, Source: "retrieval", EvidenceIDs: []string{"msg-2"}},
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
	mat := New(store, nil, nil, nil)
	items, drops, err := mat.Materialize(context.Background(), []domain.Candidate{
		{Kind: domain.GraphNodeAssertion, ID: "old", Scope: scope, Source: "retrieval"},
		{Kind: domain.GraphNodeAssertion, ID: "new", Scope: scope, Source: "retrieval"},
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

func TestMaterialize_DropsObservationAndLinkScopeViolations(t *testing.T) {
	ctx := context.Background()
	queryScope := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	siblingScope := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"}
	observations := observationstore.New()
	links := linkstore.New()
	if err := observations.Append(ctx, []domain.Observation{{
		ID:    "obs-1",
		Scope: siblingScope,
		Kind:  domain.ObservationKindEvidence,
		Text:  "sibling raw evidence",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := links.Append(ctx, []domain.FactLink{{
		ID:    "link-1",
		Scope: siblingScope,
		Type:  domain.LinkSupports,
		From:  domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: "obs-1"},
		To:    domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "fact-1"},
	}}); err != nil {
		t.Fatal(err)
	}
	mat := New(temporalstore.NewMemoryStore(), observations, links, nil)
	items, drops, err := mat.Materialize(ctx, []domain.Candidate{
		{Kind: domain.GraphNodeObservation, ID: "obs-1", Scope: queryScope, Source: "custom"},
		{Kind: domain.GraphNodeLink, ID: "link-1", Scope: queryScope, Source: "custom"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("scope-violating observation/link should not materialize, got %+v", items)
	}
	if len(drops) != 2 || drops[0].Reason != diagnostic.DropScopeViolation || drops[1].Reason != diagnostic.DropScopeViolation {
		t.Fatalf("drops = %+v, want scope violations", drops)
	}
}

func TestMaterialize_PropagatesContextCancellation(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mat := New(store, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := mat.Materialize(ctx, []domain.Candidate{{Kind: domain.GraphNodeAssertion, ID: "real", Scope: domain.Scope{RuntimeID: "rt"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context cancellation must propagate, got %v", err)
	}
}
