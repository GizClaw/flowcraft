package knowledgequality_test

import (
	"context"
	"fmt"
	"testing"

	knowledgequality "github.com/GizClaw/flowcraft/eval/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
)

const (
	corpusDir  = "testdata/corpus"
	goldenPath = "testdata/golden.jsonl"
)

// thresholds bundles the per-lane pass/fail bars used by the tests. The
// numbers are intentionally generous; the relative invariants
// (`hybrid ≥ bm25`) are what catch real regressions.
type thresholds struct {
	recall   float64 // minimum RecallAtK
	keyword  float64 // minimum KeywordRate
	negative float64 // NegativeScoreCeiling forwarded to Run
}

// runOne loads the dataset, runs a single lane through knowledgequality.Run
// and returns the LaneReport for caller assertions. The dataset load is
// cheap (100 short markdowns + 40 questions) so we re-load per test
// rather than carrying a t.Cleanup-managed cache.
func runOne(t *testing.T, lane knowledge.SearchMode, emb knowledge.Embedder, neg float64) *knowledgequality.LaneReport {
	t.Helper()
	ds, err := knowledgequality.LoadDatasetFromDir(corpusDir, goldenPath)
	if err != nil {
		t.Fatalf("load dataset: %v", err)
	}
	rep, err := knowledgequality.Run(context.Background(), ds, knowledgequality.Options{
		Embedder:             emb,
		Lanes:                []knowledgequality.Lane{lane},
		Concurrency:          4,
		NegativeScoreCeiling: neg,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := rep.Lanes[lane]
	if r == nil {
		t.Fatalf("lane %s missing from report", lane)
	}
	if r.Skipped != "" {
		t.Fatalf("lane %s skipped: %s", lane, r.Skipped)
	}
	return r
}

// assertLane logs the lane summary and asserts th. Misses /
// shortfalls / negative breaches are surfaced via t.Logf so a failing
// run pinpoints which questions regressed without re-running.
func assertLane(t *testing.T, r *knowledgequality.LaneReport, th thresholds) {
	t.Helper()
	t.Logf("lane=%s n=%d positives=%d recall@%d=%.3f (%d/%d) keyword=%.3f (%d/%d) negBreach=%d errors=%d p95=%s",
		r.Lane, r.N, r.Positives, knowledgequality.DefaultTopK,
		r.RecallAtK, r.RecallHits, r.Positives,
		r.KeywordRate, r.KeywordHits, r.RecallHits,
		r.NegativeBreach, r.Errors, r.LatencyP95)

	for _, m := range r.Misses {
		t.Logf("  MISS [%s] expected=%s topK=%v question=%q", m.ID, m.Expected, m.TopK, m.Question)
	}
	for _, k := range r.KeywordShortfalls {
		t.Logf("  KW   [%s] expected=%s missing=%v", k.ID, k.Expected, k.Missing)
	}
	if len(r.NegativeBreachIDs) > 0 {
		t.Logf("  NEG  breach=%v", r.NegativeBreachIDs)
	}

	if r.RecallAtK < th.recall {
		t.Errorf("lane=%s recall@%d=%.3f below threshold %.3f", r.Lane, knowledgequality.DefaultTopK, r.RecallAtK, th.recall)
	}
	if r.KeywordRate < th.keyword {
		t.Errorf("lane=%s keyword=%.3f below threshold %.3f", r.Lane, r.KeywordRate, th.keyword)
	}
	if r.NegativeBreach > 0 {
		t.Errorf("lane=%s negative-class breach (ceiling=%.3f): %v", r.Lane, th.negative, r.NegativeBreachIDs)
	}
}

// formatLanes is a small helper for diagnostic messages.
func formatLanes(lanes ...*knowledgequality.LaneReport) string {
	out := ""
	for _, l := range lanes {
		out += fmt.Sprintf(" %s=%.3f", l.Lane, l.RecallAtK)
	}
	return out
}
