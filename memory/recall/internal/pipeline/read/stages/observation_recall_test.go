package stages

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	observationstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/observation"
)

func TestObservationRecallDoesNotRescueOnSparseTokenOverlap(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	observations := observationstore.New()
	if err := observations.Append(ctx, []domain.Observation{{
		ID:    "obs-noise",
		Scope: scope,
		Text:  "Alice mentioned the blue cafe after dinner.",
	}}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "Where did Alice store the blue calibration capsule?", Limit: 10},
		Plan: &domain.QueryPlan{
			TotalCap: 10,
			Intent: domain.QueryIntent{Features: domain.QueryFeatures{
				Tokens: map[string]struct{}{"where": {}, "alice": {}, "store": {}, "blue": {}, "calibration": {}, "capsule": {}},
			}},
		},
		MergedItems: []domain.ContextItem{
			observationRecallSeed("seed-1"),
			observationRecallSeed("seed-2"),
			observationRecallSeed("seed-3"),
		},
	}

	detail, err := NewObservationRecall(observations).Run(ctx, state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := detail.(diagnostic.ObservationRecallDetail)
	if got.AddedObservations != 0 || len(state.MergedItems) != 3 {
		t.Fatalf("sparse lexical overlap should not rescue raw observation: detail=%+v items=%+v", got, state.MergedItems)
	}
}

func observationRecallSeed(id string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: id, Source: "retrieval", Score: 0.9},
		Fact:      domain.TemporalFact{ID: id, Content: id},
	}
}
