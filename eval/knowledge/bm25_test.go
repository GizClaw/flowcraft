package knowledgequality_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
)

// TestE2E_BM25 runs the golden set through a no-embedder Service,
// exercising only the BM25 lane. Has zero external dependencies and
// runs in CI by default. The bar is tight against today's 1.00
// baseline so any drop signals an indexer regression; paraphrase /
// cross-cluster regressions are caught instead by the relative
// `hybrid >= bm25` invariant in the integration suite.
func TestE2E_BM25(t *testing.T) {
	r := runOne(t, knowledge.ModeBM25, nil, 0)
	assertLane(t, r, thresholds{
		recall:  0.95,
		keyword: 0.95,
	})
}
