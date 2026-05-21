package stages

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
)

// scriptedMaterializer returns a pre-built slice of items + drops
// regardless of input so the test can pin exactly what lands on
// state.MaterializeDrops.
type scriptedMaterializer struct {
	items []domain.ContextItem
	drops []diagnostic.CandidateDrop
}

func (s scriptedMaterializer) Materialize(_ context.Context, _ []domain.Candidate) ([]domain.ContextItem, []diagnostic.CandidateDrop, error) {
	itemsCopy := append([]domain.ContextItem(nil), s.items...)
	dropsCopy := append([]diagnostic.CandidateDrop(nil), s.drops...)
	return itemsCopy, dropsCopy, nil
}

// TestMaterialize_PopulatesStateDrops pins the Cluster F (2026-05-21)
// contract: the materialize stage MUST surface its per-sub-scope
// CandidateDrop slice on state.MaterializeDrops in addition to the
// diagnostic detail. This is the inter-stage data channel that lets
// evolution_after_recall stay independent of state.Trace.Stages.
func TestMaterialize_PopulatesStateDrops(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	expectedDrops := []diagnostic.CandidateDrop{
		{Stage: "materialize", Reason: diagnostic.DropStaleFact, FactID: "f-stale", Source: "retrieval"},
		{Stage: "materialize", Reason: diagnostic.DropSuperseded, FactID: "f-old", Source: "entity"},
	}
	mat := scriptedMaterializer{
		items: []domain.ContextItem{{
			Candidate: domain.Candidate{FactID: "f-keep", Source: "retrieval"},
			Fact:      domain.TemporalFact{ID: "f-keep", Scope: scope, Kind: domain.KindNote},
		}},
		drops: expectedDrops,
	}

	state := &read.ReadState{
		Scope: scope,
		Now:   time.Unix(1, 0),
		Query: domain.Query{IncludeRetired: true},
		SubScopeStates: []read.SubScopeState{{
			Scope: scope,
			Fused: []domain.Candidate{{FactID: "any", Source: "retrieval"}},
		}},
	}

	stage := NewMaterialize(mat)
	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// State channel — authoritative for downstream stages.
	if !reflect.DeepEqual(state.MaterializeDrops, expectedDrops) {
		t.Fatalf("state.MaterializeDrops = %+v, want %+v", state.MaterializeDrops, expectedDrops)
	}
	// Per-sub-scope slot must stay populated too — federation_merge
	// and dashboards rely on it for per-scope attribution.
	if !reflect.DeepEqual(state.SubScopeStates[0].MaterializeDrops, expectedDrops) {
		t.Fatalf("sub.MaterializeDrops = %+v, want %+v", state.SubScopeStates[0].MaterializeDrops, expectedDrops)
	}
	// The diagnostic detail still has the same counters so trace
	// visibility is unchanged (the state channel is additive).
	md, ok := detail.(diagnostic.MaterializeDetail)
	if !ok {
		t.Fatalf("detail type = %T, want MaterializeDetail", detail)
	}
	if md.Requested != 1 || md.Returned != 1 {
		t.Fatalf("counters = %+v, want Requested=1 Returned=1", md)
	}
}

// TestMaterialize_AggregatesAcrossSubScopes pins that the top-level
// MaterializeDrops slot concatenates every sub-scope's drops in
// federation order so CollectMaterializeDrops returns a single
// uniform view to downstream consumers.
func TestMaterialize_AggregatesAcrossSubScopes(t *testing.T) {
	primary := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	sibling := domain.Scope{RuntimeID: "rt", UserID: "u2"}

	mat := scriptedMaterializer{
		drops: []diagnostic.CandidateDrop{{
			Stage: "materialize", Reason: diagnostic.DropStaleFact, FactID: "drop",
		}},
	}
	state := &read.ReadState{
		Scope: primary,
		Now:   time.Unix(1, 0),
		Query: domain.Query{IncludeRetired: true},
		SubScopeStates: []read.SubScopeState{
			{Scope: primary, Fused: []domain.Candidate{{FactID: "p"}}},
			{Scope: sibling, Fused: []domain.Candidate{{FactID: "s"}}},
		},
	}

	if _, err := NewMaterialize(mat).Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(state.MaterializeDrops); got != 2 {
		t.Fatalf("aggregated MaterializeDrops len = %d, want 2 (one per sub-scope)", got)
	}
	if state.MaterializeDrops[0].FactID != "drop" || state.MaterializeDrops[1].FactID != "drop" {
		t.Fatalf("aggregated MaterializeDrops = %+v, want both entries copied", state.MaterializeDrops)
	}
}
