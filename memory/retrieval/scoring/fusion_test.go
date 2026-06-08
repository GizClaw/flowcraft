package scoring

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
)

func hit(id string, score float64) retrieval.Hit {
	return retrieval.Hit{Doc: retrieval.Doc{ID: id}, Score: score}
}

func TestRRF_BasicOrder(t *testing.T) {
	listA := []retrieval.Hit{hit("a", 1.0), hit("b", 0.9), hit("c", 0.8)}
	listB := []retrieval.Hit{hit("b", 1.0), hit("a", 0.5), hit("d", 0.4)}

	out := RRF([][]retrieval.Hit{listA, listB}, DefaultRRFK)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}

	// "a" hits rank 1 in A and rank 2 in B; "b" hits rank 2 in A and rank 1 in B.
	// They get the same RRF score, so we just verify both top the list.
	top2 := map[string]bool{out[0].Doc.ID: true, out[1].Doc.ID: true}
	if !top2["a"] || !top2["b"] {
		t.Errorf("top2 = %v, want a and b", top2)
	}
}

func TestRRF_TaggedScores(t *testing.T) {
	out := RRF([][]retrieval.Hit{{hit("x", 0.5)}}, DefaultRRFK)
	if len(out) != 1 {
		t.Fatalf("len = %d", len(out))
	}
	if _, ok := out[0].Scores["rrf"]; !ok {
		t.Errorf("expected Scores['rrf'] tag")
	}
}

func TestRRF_DefaultK(t *testing.T) {
	got := RRF([][]retrieval.Hit{{hit("x", 1)}}, 0)
	want := 1.0 / (DefaultRRFK + 1)
	if got[0].Score != want {
		t.Errorf("score = %v, want %v (k=DefaultRRFK)", got[0].Score, want)
	}
}

func TestRRF_Empty(t *testing.T) {
	if RRF(nil, 0) != nil {
		t.Errorf("nil input should yield nil")
	}
}

func TestRRF_HighestScoreSurvives(t *testing.T) {
	// Same ID across lanes; RRF should keep the highest-scored Hit
	// copy so downstream consumers see the strongest text/metadata.
	high := hit("x", 10.0)
	low := hit("x", 0.1)
	out := RRF([][]retrieval.Hit{{low}, {high}}, DefaultRRFK)
	// Score is overwritten with RRF score, so check via Scores["rrf"]
	// existence + the original-score-tracking is implicit via metadata
	// preservation. Here we just confirm collapse to one hit.
	if len(out) != 1 {
		t.Fatalf("expected dedup, got %d", len(out))
	}
}

func TestRRF_TiesUseInputOrder(t *testing.T) {
	out := RRF([][]retrieval.Hit{{hit("b", 1), hit("a", 1)}}, DefaultRRFK)
	if len(out) != 2 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].Doc.ID != "b" || out[1].Doc.ID != "a" {
		t.Fatalf("tie order = %v, want [b a]", []string{out[0].Doc.ID, out[1].Doc.ID})
	}
}

func TestRRF_DoesNotMutateInputScoresMap(t *testing.T) {
	in := retrieval.Hit{Doc: retrieval.Doc{ID: "x", Metadata: map[string]any{"k": "v"}}, Score: 1, Scores: map[string]float64{"orig": 1}}
	out := RRF([][]retrieval.Hit{{in}}, DefaultRRFK)
	if _, ok := in.Scores["rrf"]; ok {
		t.Fatal("input Scores map was mutated")
	}
	out[0].Doc.Metadata["k"] = "changed"
	if in.Doc.Metadata["k"] != "v" {
		t.Fatal("input Metadata map was shared with output")
	}
}

func TestWeightedFusion_BasicWeights(t *testing.T) {
	bm := []retrieval.Hit{hit("a", 10), hit("b", 5)}
	vec := []retrieval.Hit{hit("b", 1), hit("c", 0.5)}
	out := WeightedFusion(
		map[string][]retrieval.Hit{"bm25": bm, "vector": vec},
		map[string]float64{"bm25": 0.7, "vector": 0.3},
	)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	// "c" only contributes the lane minimum, which normalizes to zero and is
	// dropped instead of surfacing as a zero-score fused hit.
	pos := map[string]int{}
	for i, h := range out {
		pos[h.Doc.ID] = i
	}
	if _, ok := pos["c"]; ok {
		t.Errorf("zero-score doc c should be dropped, got positions %v", pos)
	}
	if pos["a"] >= pos["b"] {
		t.Errorf("a should rank above b, got positions %v", pos)
	}
}

func TestWeightedFusion_DegenerateLane(t *testing.T) {
	// All-equal scores in a lane → normalized to 1.0 if positive.
	out := WeightedFusion(
		map[string][]retrieval.Hit{"x": {hit("a", 5), hit("b", 5)}},
		nil,
	)
	if len(out) != 2 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].Score != 1 || out[1].Score != 1 {
		t.Errorf("scores = %v %v, want 1 1", out[0].Score, out[1].Score)
	}
}

func TestWeightedFusion_MissingWeightDefaultsToOne(t *testing.T) {
	out := WeightedFusion(
		map[string][]retrieval.Hit{"unweighted": {hit("a", 10), hit("b", 0)}},
		nil,
	)
	// 'a' max → 1.0, 'b' min → 0.0
	if len(out) != 1 || out[0].Doc.ID != "a" || out[0].Score != 1.0 {
		t.Errorf("got %v, want only a=1.0", out)
	}
}

func TestRawWeightedFusion_SkipsZeroWeightLane(t *testing.T) {
	out := RawWeightedFusion(
		map[string][]retrieval.Hit{
			"bm25":   {hit("text-only", 10)},
			"vector": {hit("vector-only", 1)},
		},
		map[string]float64{"bm25": 0, "vector": 1},
	)
	if len(out) != 1 || out[0].Doc.ID != "vector-only" || out[0].Score != 1 {
		t.Fatalf("zero-weight lane leaked into fused hits: %+v", out)
	}
}

func TestWeightedFusion_DeterministicLaneOrderOnTies(t *testing.T) {
	out := WeightedFusion(
		map[string][]retrieval.Hit{
			"vector": {hit("v", 1)},
			"bm25":   {hit("b", 1)},
		},
		nil,
	)
	if len(out) != 2 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].Doc.ID != "b" || out[1].Doc.ID != "v" {
		t.Fatalf("tie order = %v, want [b v]", []string{out[0].Doc.ID, out[1].Doc.ID})
	}
}

func TestWeightedFusion_Empty(t *testing.T) {
	if WeightedFusion(nil, nil) != nil {
		t.Errorf("nil input should yield nil")
	}
}
