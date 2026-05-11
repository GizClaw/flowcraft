//go:build integration

package knowledgequality_test

import (
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/knowledge/internal/testenv"
	"github.com/GizClaw/flowcraft/sdk/knowledge"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
)

func init() { testenv.Load() }

// embedderSpec is the FLOWCRAFT alias + model to use for the
// integration lanes, e.g. "qwen:text-embedding-v4". A single
// KNOWLEDGE_EVAL_EMBEDDER variable lets the operator point this suite
// at whatever provider their .env happens to be configured for without
// having to thread three or four old-style env vars (the previous
// EMBEDDING_PROVIDER / API_KEY / MODEL trio).
const embedderSpecEnv = "KNOWLEDGE_EVAL_EMBEDDER"

// newE2EEmbedder builds the embedder via the shared env.BuildEmbedder
// resolver. Skips when KNOWLEDGE_EVAL_EMBEDDER is unset or when the
// referenced FLOWCRAFT_<ALIAS> env var is missing — the latter is the
// common case in environments that intentionally don't ship API keys.
func newE2EEmbedder(t *testing.T) knowledge.Embedder {
	t.Helper()
	spec := os.Getenv(embedderSpecEnv)
	if spec == "" {
		t.Skipf("knowledge e2e: set %s=provider:model (e.g. qwen:text-embedding-v4) to run", embedderSpecEnv)
	}
	emb, err := env.BuildEmbedder(spec)
	if err != nil {
		t.Skipf("knowledge e2e: BuildEmbedder(%q): %v", spec, err)
	}
	if emb == nil {
		t.Skipf("knowledge e2e: BuildEmbedder(%q) returned nil (FLOWCRAFT_<ALIAS> likely unset)", spec)
	}
	return emb
}

// TestE2E_Vector exercises the vector-only lane. The bar is slightly
// looser than BM25 because some hand-written questions reuse corpus
// vocabulary verbatim (BM25's home turf); vector recall still has to
// clear the same paraphrase / cross-cluster cases.
func TestE2E_Vector(t *testing.T) {
	emb := newE2EEmbedder(t)
	r := runOne(t, knowledge.ModeVector, emb, 0)
	assertLane(t, r, thresholds{
		recall:  0.80,
		keyword: 0.90,
	})
}

// TestE2E_Hybrid runs both lanes and fuses via RRF — the highest bar
// because hybrid ought to beat each lane individually on a varied
// query mix.
func TestE2E_Hybrid(t *testing.T) {
	emb := newE2EEmbedder(t)
	r := runOne(t, knowledge.ModeHybrid, emb, 0)
	assertLane(t, r, thresholds{
		recall:  0.95,
		keyword: 0.95,
	})
}

// TestE2E_HybridBeatsBM25 is the relative invariant: regressions that
// degrade hybrid below pure BM25 are usually fusion bugs (wrong RRF
// constant, dropped lane, ranker misorder) and absolute thresholds
// won't catch them. We run both lanes against the same Service so any
// quality drift between runs is shared.
func TestE2E_HybridBeatsBM25(t *testing.T) {
	emb := newE2EEmbedder(t)
	bm25 := runOne(t, knowledge.ModeBM25, emb, 0)
	hybrid := runOne(t, knowledge.ModeHybrid, emb, 0)

	// Allow a 5% downward wobble for stochastic fusion edge cases; a
	// real regression typically drops by 10%+.
	const tolerance = 0.05
	if hybrid.RecallAtK+tolerance < bm25.RecallAtK {
		t.Errorf("hybrid recall regressed below bm25:%s (tolerance=%.2f)",
			formatLanes(bm25, hybrid), tolerance)
	}
}
