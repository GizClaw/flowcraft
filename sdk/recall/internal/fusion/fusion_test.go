package fusion

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestWeightedRRF_DedupesAndCombinesScores(t *testing.T) {
	results := []domain.SourceResult{
		{
			Source: "retrieval",
			Candidates: []domain.Candidate{
				{FactID: "a", Source: "retrieval", Rank: 1},
				{FactID: "b", Source: "retrieval", Rank: 2},
			},
		},
		{
			Source: "entity",
			Candidates: []domain.Candidate{
				{FactID: "a", Source: "entity", Rank: 1},
				{FactID: "c", Source: "entity", Rank: 2},
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
	if fused[0].FactID != "a" {
		t.Errorf("top hit should be 'a' (matches both sources), got %+v", fused[0])
	}
	srcs, _ := fused[0].Metadata["sources"].([]string)
	if len(srcs) != 2 {
		t.Errorf("multi-source membership not recorded: %v", srcs)
	}
}

func TestWeightedRRF_PerSourceCapEmitsDrops(t *testing.T) {
	results := []domain.SourceResult{
		{
			Source: "retrieval",
			Candidates: []domain.Candidate{
				{FactID: "a", Source: "retrieval", Rank: 1},
				{FactID: "b", Source: "retrieval", Rank: 2},
				{FactID: "c", Source: "retrieval", Rank: 3},
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

func TestWeightedRRF_OutlierBoost_RescuesRareTokenMatch(t *testing.T) {
	// Models a rare-token retrieval scenario:
	//
	// - retrieval lane returns one rare-token outlier ("rare_fact")
	//   with a BM25 score ~7x the median of the rest of the lane
	// - several mid-rank candidates appear in BOTH retrieval and
	//   entity lanes (the multi-source corroboration that vanilla RRF
	//   rewards), with BM25 scores around the lane's typical noise
	//
	// Without the outlier boost, RRF prefers the multi-source
	// candidate because rank-based aggregation discards BM25's
	// magnitude. With the boost, the within-source rank-1 outlier's
	// contribution is amplified just enough to overcome the
	// dual-source rank-2 corroboration — the rare-token match wins.
	retrievalCands := []domain.Candidate{
		{FactID: "rare_fact", Source: "retrieval", Rank: 1, Score: 14.0},
		{FactID: "filler1", Source: "retrieval", Rank: 2, Score: 2.0},
		{FactID: "filler2", Source: "retrieval", Rank: 3, Score: 2.0},
		{FactID: "filler3", Source: "retrieval", Rank: 4, Score: 2.0},
		{FactID: "filler4", Source: "retrieval", Rank: 5, Score: 2.0},
	}
	entityCands := []domain.Candidate{
		{FactID: "filler1", Source: "entity", Rank: 1, Score: 1.0},
	}
	results := []domain.SourceResult{
		{Source: "retrieval", Candidates: retrievalCands},
		{Source: "entity", Candidates: entityCands},
	}

	// Baseline (boost disabled): filler1 wins because it's
	// multi-source corroborated.
	fused, _, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		Weights:         map[string]float64{"retrieval": 1.0, "entity": 1.0},
		OutlierBoostCap: 1.0, // disable boost
	})
	if fused[0].FactID != "filler1" {
		t.Fatalf("baseline: expected multi-source 'filler1' to win without boost, got %+v", fused[0])
	}

	// With boost on: the rare-token outlier wins.
	fused, _, _ = WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		Weights: map[string]float64{"retrieval": 1.0, "entity": 1.0},
		// rely on defaults (cap=2.0, threshold=2.0, max-rank=5)
	})
	if fused[0].FactID != "rare_fact" {
		t.Errorf("with boost: expected rare-token 'rare_fact' to win, got rank order: %+v",
			func() []string {
				out := []string{}
				for _, c := range fused {
					out = append(out, c.FactID)
				}
				return out
			}())
	}
}

func TestWeightedRRF_OutlierBoost_NoOpOnUniformScores(t *testing.T) {
	// Sources whose candidates all share the same score (entity /
	// graph / profile in presence-signal mode) should NOT receive
	// boosts — there's no magnitude signal to amplify. We verify the
	// boost factor stays at 1 by checking that fused scores equal
	// the plain RRF formula.
	results := []domain.SourceResult{
		{
			Source: "entity",
			Candidates: []domain.Candidate{
				{FactID: "a", Source: "entity", Rank: 1, Score: 1.0},
				{FactID: "b", Source: "entity", Rank: 2, Score: 1.0},
				{FactID: "c", Source: "entity", Rank: 3, Score: 1.0},
			},
		},
	}
	fused, _, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		Weights: map[string]float64{"entity": 1.0},
	})
	wantTop := 1.0 / float64(DefaultRRFK+1)
	if fused[0].FactID != "a" || fused[0].Score != wantTop {
		t.Errorf("uniform-score source should not boost top: got %+v, want top=a with score=%v", fused[0], wantTop)
	}
}

func TestWeightedRRF_TotalCapEmitsDrops(t *testing.T) {
	results := []domain.SourceResult{
		{
			Source: "retrieval",
			Candidates: []domain.Candidate{
				{FactID: "a", Source: "retrieval", Rank: 1},
				{FactID: "b", Source: "retrieval", Rank: 2},
				{FactID: "c", Source: "retrieval", Rank: 3},
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
				{FactID: "retrieval-evidence", Source: "retrieval", Rank: 1},
			},
		},
		{
			Source: "entity",
			Candidates: []domain.Candidate{
				{FactID: "multi-source-distractor", Source: "entity", Rank: 1},
			},
		},
		{
			Source: "graph",
			Candidates: []domain.Candidate{
				{FactID: "multi-source-distractor", Source: "graph", Rank: 1},
			},
		},
	}

	withoutFloor, _, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		TotalCap:     1,
		SourceFloors: map[string]int{}, // explicit opt-out
	})
	if withoutFloor[0].FactID != "multi-source-distractor" {
		t.Fatalf("without floor expected multi-source distractor to win, got %+v", withoutFloor)
	}

	withFloor, drops, _ := WeightedRRF{}.Fuse(context.Background(), results, port.FusionOptions{
		TotalCap: 1,
	})
	if withFloor[0].FactID != "retrieval-evidence" {
		t.Fatalf("retrieval floor should keep top retrieval evidence, got %+v", withFloor)
	}
	if len(drops) != 1 || drops[0].FactID != "multi-source-distractor" {
		t.Fatalf("drops = %+v, want distractor dropped by total cap", drops)
	}
}
