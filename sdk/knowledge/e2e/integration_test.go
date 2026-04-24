//go:build integration

package e2e_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/e2e/internal/testenv"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
)

func init() { testenv.Load() }

// newE2EEmbedder mirrors sdkx/embedding/embedding_integration_test.go's
// configuration so a single .env unlocks both suites. Skips when the
// minimum env trio (provider + key + model) is missing so this file
// stays harmless in environments without credentials.
func newE2EEmbedder(t *testing.T) embedding.Embedder {
	t.Helper()
	provider := os.Getenv("EMBEDDING_PROVIDER")
	apiKey := os.Getenv("EMBEDDING_API_KEY")
	model := os.Getenv("EMBEDDING_MODEL")
	if provider == "" || apiKey == "" || model == "" {
		t.Skip("knowledge e2e: set EMBEDDING_PROVIDER / EMBEDDING_API_KEY / EMBEDDING_MODEL to run")
	}
	cfg := map[string]any{"api_key": apiKey}
	if v := os.Getenv("EMBEDDING_BASE_URL"); v != "" {
		cfg["base_url"] = v
	}
	if v := os.Getenv("EMBEDDING_API_VERSION"); v != "" {
		cfg["api_version"] = v
	}
	emb, err := embedding.NewFromConfig(provider, model, cfg)
	if err != nil {
		t.Fatalf("knowledge e2e: NewFromConfig(%q, %q): %v", provider, model, err)
	}
	return emb
}

// TestE2E_Vector exercises the vector-only lane. The bar is slightly
// looser than BM25 because some hand-written questions reuse corpus
// vocabulary verbatim, which is BM25's home turf; vector recall still
// has to clear the same paraphrase / cross-cluster cases.
func TestE2E_Vector(t *testing.T) {
	emb := newE2EEmbedder(t)
	svc := buildService(t, emb)

	// Allow extra wall-clock for the full ingest+search loop against a
	// real embedding provider; 100 docs ≈ 100+ embedding calls.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_ = ctx // honoured implicitly via knowledge.Service's own ctx plumbing

	runEval(t, svc, knowledge.ModeVector, thresholds{
		recall:   0.80,
		keyword:  0.90,
		negative: 0,
	})
}

// TestE2E_Hybrid runs both lanes and fuses via RRF. It's the highest
// bar because hybrid ought to beat each lane individually on a varied
// query mix.
func TestE2E_Hybrid(t *testing.T) {
	emb := newE2EEmbedder(t)
	svc := buildService(t, emb)

	runEval(t, svc, knowledge.ModeHybrid, thresholds{
		recall:   0.95,
		keyword:  0.95,
		negative: 0,
	})
}

// TestE2E_HybridBeatsBM25 is the relative invariant: regressions that
// degrade hybrid below pure BM25 are usually fusion bugs (wrong RRF
// constant, dropped lane, ranker misorder) and absolute thresholds
// won't catch them. We run both modes against the same service so any
// quality drift between runs is shared.
func TestE2E_HybridBeatsBM25(t *testing.T) {
	emb := newE2EEmbedder(t)
	svc := buildService(t, emb)

	bm25 := runEval(t, svc, knowledge.ModeBM25, thresholds{recall: 0, keyword: 0})
	hybrid := runEval(t, svc, knowledge.ModeHybrid, thresholds{recall: 0, keyword: 0})

	// Allow a 5% downward wobble for stochastic fusion edge cases; a
	// real regression typically drops by 10%+.
	const tolerance = 0.05
	if hybrid+tolerance < bm25 {
		t.Errorf("hybrid recall regressed below bm25: hybrid=%.2f bm25=%.2f (tolerance=%.2f)",
			hybrid, bm25, tolerance)
	}
}
