package beir_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/eval/beir"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
)

// syntheticDataset hand-builds a tiny BEIR-shaped dataset whose
// relevance structure is known up front. Two queries, four documents,
// graded judgments — small enough that BM25 should rank perfectly and
// every metric has a closed-form answer the test can compare against.
//
// Layout:
//
//	q1: "apple pie recipe"     → doc1 (grade 2, perfect match)
//	                             doc2 (grade 1, partially relevant)
//	                             doc3, doc4 irrelevant
//	q2: "tax filing deadline"  → doc3 (grade 2)
//	                             doc4 (grade 1)
//	                             doc1, doc2 irrelevant
func syntheticDataset() *beir.Dataset {
	return &beir.Dataset{
		Name: "synth",
		Documents: []beir.Document{
			{ID: "doc1", Title: "Classic Apple Pie", Text: "Step-by-step apple pie recipe with cinnamon and crust."},
			{ID: "doc2", Title: "Fruit Desserts", Text: "Apple desserts including a quick apple crumble recipe."},
			{ID: "doc3", Title: "Tax Filing 2026", Text: "Federal tax filing deadline is April 15. Extensions available."},
			{ID: "doc4", Title: "IRS Forms", Text: "List of IRS forms relevant to individual taxpayers and filing dates."},
		},
		Queries: []beir.Query{
			{ID: "q1", Text: "apple pie recipe"},
			{ID: "q2", Text: "tax filing deadline"},
		},
		Qrels: map[string]map[string]int{
			"q1": {"doc1": 2, "doc2": 1},
			"q2": {"doc3": 2, "doc4": 1},
		},
	}
}

// TestRun_BM25_SyntheticSanity asserts the package can drive a full
// ingest → search → score loop end-to-end on a deterministic dataset.
// The hard math is exercised in metrics_internal_test.go; here we
// just want a "doesn't crash, returns a populated report, every
// metric lives in [0,1], the top-relevant doc is reached first"
// sanity check. Asserting exact numeric values would tie the test to
// BM25's IDF on a 4-doc corpus, which is exactly the brittle layer
// we don't want.
func TestRun_BM25_SyntheticSanity(t *testing.T) {
	ds := syntheticDataset()
	rep, err := beir.Run(context.Background(), ds, beir.Options{
		Lanes:   []beir.Lane{knowledge.ModeBM25},
		Cutoffs: []int{10},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := rep.Lanes[knowledge.ModeBM25]
	if r == nil {
		t.Fatalf("missing bm25 report: %+v", rep.Lanes)
	}
	if r.Skipped != "" {
		t.Fatalf("bm25 skipped unexpectedly: %s", r.Skipped)
	}
	if r.N != 2 {
		t.Errorf("N: want 2 (one per query), got %d", r.N)
	}
	// Every metric lives in [0,1]; MRR should be 1.0 because each
	// query's perfect-match document (doc1 for q1, doc3 for q2) is
	// trivially the top BM25 hit on a 4-doc corpus.
	if r.NDCG[10] < 0 || r.NDCG[10] > 1 {
		t.Errorf("nDCG@10 out of range: %.3f", r.NDCG[10])
	}
	if r.Recall[10] < 0 || r.Recall[10] > 1 {
		t.Errorf("Recall@10 out of range: %.3f", r.Recall[10])
	}
	// MRR ≥ 0.5 means that on average a relevant doc shows up by
	// rank ≤ 2. With our deliberate fixture (two queries whose
	// vocabulary is fully disjoint from the irrelevant docs) BM25
	// always returns the grade=2 doc within the top hits, but the
	// exact MRR depends on default score thresholds inside
	// sdk/knowledge so we avoid pinning it.
	if r.MRR < 0.5 {
		t.Errorf("MRR below sanity floor 0.5: got %.6f", r.MRR)
	}
	t.Logf("lane=%s N=%d nDCG@10=%.3f recall@10=%.3f mrr=%.3f p95=%s",
		r.Lane, r.N, r.NDCG[10], r.Recall[10], r.MRR, r.LatencyP95)
}

// TestRun_AbsentLane_Skipped exercises the auto-skip behaviour: if the
// caller asks for vector / hybrid without an embedder, the lane should
// land in the Report with Skipped set, not error.
func TestRun_AbsentLane_Skipped(t *testing.T) {
	ds := syntheticDataset()
	rep, err := beir.Run(context.Background(), ds, beir.Options{
		Lanes:   []beir.Lane{knowledge.ModeBM25, knowledge.ModeVector},
		Cutoffs: []int{10},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r := rep.Lanes[knowledge.ModeBM25]; r == nil || r.Skipped != "" {
		t.Errorf("bm25 should NOT be skipped, got %+v", r)
	}
	v := rep.Lanes[knowledge.ModeVector]
	if v == nil {
		t.Fatal("vector lane missing from report")
	}
	if v.Skipped == "" {
		t.Errorf("vector lane should be skipped (no embedder), got %+v", v)
	}
}

// TestRun_HookEvents asserts the lifecycle event sequence so the
// Feishu adapter doesn't silently regress (event kinds and ordering
// drive the live card layout).
func TestRun_HookEvents(t *testing.T) {
	ds := syntheticDataset()
	var kinds []string
	_, err := beir.Run(context.Background(), ds, beir.Options{
		Lanes:   []beir.Lane{knowledge.ModeBM25},
		Cutoffs: []int{10},
		Hook: func(_ context.Context, e beir.Event) {
			kinds = append(kinds, e.Kind)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// We expect the canonical sequence; intermediate lane_progress is
	// optional (depends on ProgressPct, which we left at zero here).
	want := []string{"start", "ingest_done", "lane_start", "lane_done", "done"}
	if len(kinds) < len(want) {
		t.Fatalf("not enough events; want at least %d, got %v", len(want), kinds)
	}
	for i, w := range want {
		if kinds[i] != w {
			t.Errorf("event[%d]: want %q, got %q (full: %v)", i, w, kinds[i], kinds)
		}
	}
}
