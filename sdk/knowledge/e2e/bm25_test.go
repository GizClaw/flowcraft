package e2e_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
)

// TestE2E_BM25 runs the full golden set through the local stack with no
// embedder configured, exercising only the BM25 lane. This case has
// zero external dependency and runs in CI by default. The bar is
// intentionally lenient: paraphrase / cross-cluster queries may miss
// because BM25 is keyword-bound, and that's fine — the relative
// hybrid≥bm25 invariant in the integration suite is what tightens the
// screw.
func TestE2E_BM25(t *testing.T) {
	svc := buildService(t, nil)
	// Empirically, the CJK tokenizer + this 100-doc fixture lets BM25
	// land 33/36 at rank=1, 2 at rank=2, 1 at rank=4 — even after we
	// added 10 deliberately reworded questions ("g101"–"g110") that
	// avoid the doc's headline vocabulary. Single-CJK-character matching
	// is just that strong on short Chinese paragraphs. The integration
	// suite's relative `hybrid >= bm25` invariant is what catches lane /
	// fusion regressions; BM25's absolute bar here is set tight against
	// today's 1.00 baseline so any drop signals an indexer regression.
	runEval(t, svc, knowledge.ModeBM25, thresholds{
		recall:   0.95,
		keyword:  0.95,
		negative: 0, // BM25 negatives can score arbitrarily; only the integration suite asserts a ceiling
	})
}
