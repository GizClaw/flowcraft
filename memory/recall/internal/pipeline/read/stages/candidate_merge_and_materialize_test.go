package stages

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
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

type passthroughFuser struct{}

func (passthroughFuser) Fuse(_ context.Context, results []domain.SourceResult, _ port.FusionOptions) ([]domain.Candidate, []diagnostic.CandidateDrop, error) {
	var out []domain.Candidate
	for _, res := range results {
		out = append(out, res.Candidates...)
	}
	return out, nil, nil
}

func TestCandidateMergeAndMaterializePopulatesStateDrops(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	expectedDrops := []diagnostic.CandidateDrop{
		{Stage: "candidate_materialize", Reason: diagnostic.DropStaleFact, FactID: "f-stale", Source: "retrieval"},
		{Stage: "candidate_materialize", Reason: diagnostic.DropSuperseded, FactID: "f-old", Source: "entity"},
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
		Plan:  &domain.QueryPlan{TotalCap: 10},
		SubScopeStates: []read.SubScopeState{{
			Scope: scope,
			SourceResults: []domain.SourceResult{{
				Source:     "retrieval",
				Candidates: []domain.Candidate{{FactID: "any", Source: "retrieval"}},
			}},
		}},
	}

	stage := NewCandidateMergeAndMaterialize(passthroughFuser{}, port.FusionOptions{}, nil, mat)
	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// State channel — authoritative for downstream stages.
	if !reflect.DeepEqual(state.MaterializeDrops, expectedDrops) {
		t.Fatalf("state.MaterializeDrops = %+v, want %+v", state.MaterializeDrops, expectedDrops)
	}
	// Per-sub-scope slot must stay populated too; diagnostics rely on it for
	// per-scope attribution.
	if !reflect.DeepEqual(state.SubScopeStates[0].MaterializeDrops, expectedDrops) {
		t.Fatalf("sub.MaterializeDrops = %+v, want %+v", state.SubScopeStates[0].MaterializeDrops, expectedDrops)
	}
	md, ok := detail.(diagnostic.CandidateMergeAndMaterializeDetail)
	if !ok {
		t.Fatalf("detail type = %T, want CandidateMergeAndMaterializeDetail", detail)
	}
	if md.InputCount != 1 || md.MaterializedCount != 1 {
		t.Fatalf("counters = %+v, want InputCount=1 Materialized=1", md)
	}
}

func TestCandidateMergeAndMaterializeAggregatesDropsAcrossSubScopes(t *testing.T) {
	primary := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	sibling := domain.Scope{RuntimeID: "rt", UserID: "u2"}

	mat := scriptedMaterializer{
		drops: []diagnostic.CandidateDrop{{
			Stage: "candidate_materialize", Reason: diagnostic.DropStaleFact, FactID: "drop",
		}},
	}
	state := &read.ReadState{
		Scope: primary,
		Now:   time.Unix(1, 0),
		Query: domain.Query{IncludeRetired: true},
		Plan:  &domain.QueryPlan{TotalCap: 10},
		SubScopeStates: []read.SubScopeState{
			{Scope: primary, SourceResults: []domain.SourceResult{{Source: "retrieval", Candidates: []domain.Candidate{{FactID: "p"}}}}},
			{Scope: sibling, SourceResults: []domain.SourceResult{{Source: "retrieval", Candidates: []domain.Candidate{{FactID: "s"}}}}},
		},
	}

	if _, err := NewCandidateMergeAndMaterialize(passthroughFuser{}, port.FusionOptions{}, nil, mat).Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(state.MaterializeDrops); got != 2 {
		t.Fatalf("aggregated MaterializeDrops len = %d, want 2 (one per sub-scope)", got)
	}
	if state.MaterializeDrops[0].FactID != "drop" || state.MaterializeDrops[1].FactID != "drop" {
		t.Fatalf("aggregated MaterializeDrops = %+v, want both entries copied", state.MaterializeDrops)
	}
}
