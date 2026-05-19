package fusion

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

func TestWeightedRRF_DedupesAndCombinesScores(t *testing.T) {
	results := []model.SourceResult{
		{
			Source: "retrieval",
			Candidates: []model.Candidate{
				{FactID: "a", Source: "retrieval", Rank: 1},
				{FactID: "b", Source: "retrieval", Rank: 2},
			},
		},
		{
			Source: "entity",
			Candidates: []model.Candidate{
				{FactID: "a", Source: "entity", Rank: 1},
				{FactID: "c", Source: "entity", Rank: 2},
			},
		},
	}
	fused, drops, err := WeightedRRF{}.Fuse(context.Background(), results, Options{
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
	results := []model.SourceResult{
		{
			Source: "retrieval",
			Candidates: []model.Candidate{
				{FactID: "a", Source: "retrieval", Rank: 1},
				{FactID: "b", Source: "retrieval", Rank: 2},
				{FactID: "c", Source: "retrieval", Rank: 3},
			},
		},
	}
	fused, drops, _ := WeightedRRF{}.Fuse(context.Background(), results, Options{
		PerSourceCap: 2,
	})
	if len(fused) != 2 {
		t.Errorf("expected per-source cap to trim to 2, got %d", len(fused))
	}
	if len(drops) != 1 || drops[0].Reason != model.DropPerSourceCap {
		t.Errorf("drops = %+v", drops)
	}
}

func TestWeightedRRF_TotalCapEmitsDrops(t *testing.T) {
	results := []model.SourceResult{
		{
			Source: "retrieval",
			Candidates: []model.Candidate{
				{FactID: "a", Source: "retrieval", Rank: 1},
				{FactID: "b", Source: "retrieval", Rank: 2},
				{FactID: "c", Source: "retrieval", Rank: 3},
			},
		},
	}
	fused, drops, _ := WeightedRRF{}.Fuse(context.Background(), results, Options{
		TotalCap: 1,
	})
	if len(fused) != 1 {
		t.Errorf("expected total cap to trim to 1, got %d", len(fused))
	}
	if len(drops) != 2 {
		t.Errorf("expected 2 total-cap drops, got %+v", drops)
	}
}
