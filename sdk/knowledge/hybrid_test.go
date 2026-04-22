package knowledge

import (
	"testing"
)

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	sim := cosineSimilarity(a, b)
	if sim < 0.99 {
		t.Fatalf("expected ~1.0, got %f", sim)
	}

	c := []float32{0, 1, 0}
	sim2 := cosineSimilarity(a, c)
	if sim2 > 0.01 {
		t.Fatalf("expected ~0.0, got %f", sim2)
	}
}

func TestCosineSimilarity_Empty(t *testing.T) {
	if cosineSimilarity(nil, nil) != 0 {
		t.Fatal("expected 0 for nil inputs")
	}
	if cosineSimilarity([]float32{1}, []float32{1, 2}) != 0 {
		t.Fatal("expected 0 for mismatched lengths")
	}
}

func TestRRFMerge(t *testing.T) {
	bm25 := []SearchResult{
		{DocName: "a.md", ChunkIndex: 0, Score: 3.0},
		{DocName: "b.md", ChunkIndex: 0, Score: 2.0},
	}
	semantic := []SearchResult{
		{DocName: "b.md", ChunkIndex: 0, Score: 0.9},
		{DocName: "c.md", ChunkIndex: 0, Score: 0.8},
	}
	merged := RRFMerge(bm25, semantic, 60)
	if len(merged) != 3 {
		t.Fatalf("expected 3 merged results, got %d", len(merged))
	}

	var bScore float64
	for _, r := range merged {
		if r.DocName == "b.md" {
			bScore = r.Score
		}
	}
	if bScore <= 0 {
		t.Fatal("expected b.md to have positive RRF score")
	}
}

func TestRRFMerge_Empty(t *testing.T) {
	merged := RRFMerge(nil, nil, 60)
	if len(merged) != 0 {
		t.Fatalf("expected 0 results, got %d", len(merged))
	}
}
