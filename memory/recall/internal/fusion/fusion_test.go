package fusion

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestWeightedRRF_DedupesAndRecordsProjectionRoutes(t *testing.T) {
	results := []domain.SourceResult{
		{
			Source: "retrieval",
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "a", Source: "retrieval", Rank: 1},
				{Kind: domain.GraphNodeAssertion, ID: "b", Source: "retrieval", Rank: 2},
			},
		},
		{
			Source: "entity",
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "a", Source: "entity", Rank: 1},
				{Kind: domain.GraphNodeAssertion, ID: "c", Source: "entity", Rank: 2},
			},
		},
	}
	fused, drops, err := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		Weights: map[string]float64{"retrieval": 1.0, "entity": 0.8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(drops) != 0 {
		t.Errorf("no drops expected, got %+v", drops)
	}
	if len(fused) != 3 {
		t.Fatalf("expected 3 unique facts, got %d (%+v)", len(fused), fused)
	}
	if fused[0].ID != "a" {
		t.Errorf("top hit should keep the best single route for 'a', got %+v", fused[0])
	}
	srcs, _ := fused[0].Metadata["sources"].([]string)
	if len(srcs) != 2 {
		t.Errorf("multi-source membership not recorded: %v", srcs)
	}
}

func TestWeightedRRF_MergesEvidenceIDsForSameFact(t *testing.T) {
	results := []domain.SourceResult{{
		Source: "retrieval",
		Candidates: []domain.Candidate{
			{Kind: domain.GraphNodeAssertion, ID: "a", Source: "retrieval", Rank: 1, EvidenceIDs: []string{"e1"}},
			{Kind: domain.GraphNodeAssertion, ID: "a", Source: "retrieval", Rank: 2, EvidenceIDs: []string{"e2"}},
		},
	}}
	fused, _, err := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fused) != 1 {
		t.Fatalf("expected one fused candidate, got %+v", fused)
	}
	if got := fused[0].EvidenceIDs; len(got) != 2 || got[0] != "e1" || got[1] != "e2" {
		t.Fatalf("evidence ids = %+v, want [e1 e2]", got)
	}
}

func TestWeightedRRF_DoesNotMergeDifferentKindsWithSameID(t *testing.T) {
	results := []domain.SourceResult{
		{Source: "retrieval", Candidates: []domain.Candidate{
			{Kind: domain.GraphNodeAssertion, ID: "same", Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"}, Source: "retrieval", Rank: 1},
		}},
		{Source: "observation", Candidates: []domain.Candidate{
			{Kind: domain.GraphNodeObservation, ID: "same", Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"}, Source: "observation", Rank: 1},
		}},
	}
	fused, _, err := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fused) != 2 {
		t.Fatalf("different candidate kinds with same ID must not merge, got %+v", fused)
	}
}

func TestWeightedRRF_PerSourceCapEmitsDrops(t *testing.T) {
	results := []domain.SourceResult{
		{
			Source: "retrieval",
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "a", Source: "retrieval", Rank: 1},
				{Kind: domain.GraphNodeAssertion, ID: "b", Source: "retrieval", Rank: 2},
				{Kind: domain.GraphNodeAssertion, ID: "c", Source: "retrieval", Rank: 3},
			},
		},
	}
	fused, drops, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		PerSourceCap: 2,
	})
	if len(fused) != 2 {
		t.Errorf("expected per-source cap to trim to 2, got %d", len(fused))
	}
	if len(drops) != 1 || drops[0].Reason != diagnostic.DropPerSourceCap {
		t.Errorf("drops = %+v", drops)
	}
}

func TestWeightedRRF_DoesNotLetProjectionCountBeatTopRetrieval(t *testing.T) {
	retrievalCands := []domain.Candidate{
		{Kind: domain.GraphNodeAssertion, ID: "rare_fact", Source: "retrieval", Rank: 1, Score: 14.0},
		{Kind: domain.GraphNodeAssertion, ID: "filler1", Source: "retrieval", Rank: 2, Score: 2.0},
		{Kind: domain.GraphNodeAssertion, ID: "filler2", Source: "retrieval", Rank: 3, Score: 2.0},
		{Kind: domain.GraphNodeAssertion, ID: "filler3", Source: "retrieval", Rank: 4, Score: 2.0},
		{Kind: domain.GraphNodeAssertion, ID: "filler4", Source: "retrieval", Rank: 5, Score: 2.0},
	}
	entityCands := []domain.Candidate{
		{Kind: domain.GraphNodeAssertion, ID: "filler1", Source: "entity", Rank: 1, Score: 1.0},
	}
	results := []domain.SourceResult{
		{Source: "retrieval", Candidates: retrievalCands},
		{Source: "entity", Candidates: entityCands},
	}

	fused, _, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		Weights: map[string]float64{"retrieval": 1.0, "entity": 1.0},
	})
	if fused[0].ID != "rare_fact" {
		t.Fatalf("expected top retrieval route to win without projection-count boost, got rank order: %+v",
			func() []string {
				out := []string{}
				for _, c := range fused {
					out = append(out, c.ID)
				}
				return out
			}())
	}
}

func TestWeightedRRF_IgnoresUniformSourceScores(t *testing.T) {
	results := []domain.SourceResult{
		{
			Source: "entity",
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "a", Source: "entity", Rank: 1, Score: 1.0},
				{Kind: domain.GraphNodeAssertion, ID: "b", Source: "entity", Rank: 2, Score: 1.0},
				{Kind: domain.GraphNodeAssertion, ID: "c", Source: "entity", Rank: 3, Score: 1.0},
			},
		},
	}
	fused, _, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		Weights: map[string]float64{"entity": 1.0},
	})
	wantTop := 1.0 / float64(DefaultRRFK+1)
	if fused[0].ID != "a" || fused[0].Score != wantTop {
		t.Errorf("uniform-score source should not boost top: got %+v, want top=a with score=%v", fused[0], wantTop)
	}
}

func TestWeightedRRF_TotalCapEmitsDrops(t *testing.T) {
	results := []domain.SourceResult{
		{
			Source: "retrieval",
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "a", Source: "retrieval", Rank: 1},
				{Kind: domain.GraphNodeAssertion, ID: "b", Source: "retrieval", Rank: 2},
				{Kind: domain.GraphNodeAssertion, ID: "c", Source: "retrieval", Rank: 3},
			},
		},
	}
	fused, drops, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		TotalCap: 1,
	})
	if len(fused) != 1 {
		t.Errorf("expected total cap to trim to 1, got %d", len(fused))
	}
	if len(drops) != 2 {
		t.Errorf("expected 2 total-cap drops, got %+v", drops)
	}
}

func TestWeightedRRF_RetrievalFloorProtectsStrongSingleSourceEvidence(t *testing.T) {
	results := []domain.SourceResult{
		{
			Source: "retrieval",
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "retrieval-evidence", Source: "retrieval", Rank: 1},
			},
		},
		{
			Source: "entity",
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "multi-source-distractor", Source: "entity", Rank: 1},
			},
		},
		{
			Source: "graph",
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "multi-source-distractor", Source: "graph", Rank: 1},
			},
		},
	}

	withoutFloor, _, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		TotalCap:     1,
		SourceFloors: map[string]int{}, // explicit opt-out
	})
	if withoutFloor[0].ID != "retrieval-evidence" {
		t.Fatalf("projection count should not beat top retrieval even without floor, got %+v", withoutFloor)
	}

	withFloor, drops, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		TotalCap: 1,
	})
	if withFloor[0].ID != "retrieval-evidence" {
		t.Fatalf("retrieval floor should keep top retrieval evidence, got %+v", withFloor)
	}
	if len(drops) != 1 || drops[0].FactID != "multi-source-distractor" {
		t.Fatalf("drops = %+v, want distractor dropped by total cap", drops)
	}
}
