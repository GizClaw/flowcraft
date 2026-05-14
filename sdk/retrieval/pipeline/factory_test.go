package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// fakeEmbedder is a deterministic embedder for tests: returns the
// dot-product-friendly vector encoded in the input via a fixed
// vocabulary so we can ask vector recall to favour known docs.
type fakeEmbedder struct {
	vocab map[string][]float32
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := f.vocab[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 0}, nil
}

func (f *fakeEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := f.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Compile-time assertion that fakeEmbedder still satisfies the SDK
// interface — guards against drift in embedding.Embedder.
var _ embedding.Embedder = (*fakeEmbedder)(nil)

// TestLTMMultiRecall_StageTopology asserts that WithMultiRecall
// flips the LTM pipeline from "Retrieve+liftRecall" to
// "MultiRetrieve+RRFFusion" and that the entity lane is wired in.
func TestLTMMultiRecall_StageTopology(t *testing.T) {
	emb := &fakeEmbedder{vocab: map[string][]float32{}}

	legacy := LTM(emb)
	legacyNames := stageNames(legacy)
	if containsName(legacyNames, "MultiRetrieve") {
		t.Fatalf("legacy LTM should not include MultiRetrieve: %v", legacyNames)
	}
	if !containsNamePrefix(legacyNames, "Retrieve(") {
		t.Fatalf("legacy LTM should include a single-lane Retrieve: %v", legacyNames)
	}

	multi := LTM(emb, WithMultiRecall(true))
	multiNames := stageNames(multi)
	if !containsName(multiNames, "MultiRetrieve") {
		t.Fatalf("multi-recall LTM should include MultiRetrieve: %v", multiNames)
	}
	if !containsName(multiNames, "RRFFusion") {
		t.Fatalf("multi-recall LTM should include RRFFusion: %v", multiNames)
	}
	// Legacy lift step is replaced under multi-recall — RRFFusion
	// writes to Fused and post stages pick it up via pickFinalish.
	if containsNamePrefix(multiNames, "Lift(") {
		t.Fatalf("multi-recall LTM should not lift a single lane: %v", multiNames)
	}
	if containsName(multiNames, "BM25Boost") {
		t.Fatalf("multi-recall LTM should suppress BM25Boost (now a recall lane): %v", multiNames)
	}
	if !containsName(multiNames, "EntityBoost") {
		t.Fatalf("multi-recall LTM should keep EntityBoost (re-ranks fused list): %v", multiNames)
	}
}

// TestLTMMultiRecall_RunsEndToEnd exercises the multi-recall pipeline
// against an in-memory index seeded with one entity-perfect, one
// vector-perfect, and one no-match doc, asserting that fusion
// surfaces the two relevant docs ahead of the irrelevant one.
func TestLTMMultiRecall_RunsEndToEnd(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()

	emb := &fakeEmbedder{vocab: map[string][]float32{
		// Query "favourite coffee" embeds as the same vector as
		// docB so vector recall pulls docB first.
		"favourite coffee": {1, 0, 0},
	}}

	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{
			ID:        "entity-hit",
			Content:   "matcha discussion at the cafe",
			Vector:    []float32{0, 0, 1}, // off-axis from query
			Metadata:  map[string]any{"entities": []any{"alice"}},
			Timestamp: now,
		},
		{
			ID:        "vector-hit",
			Content:   "unrelated lexically",
			Vector:    []float32{1, 0, 0}, // on-axis with query vector
			Timestamp: now,
		},
		{
			ID:        "noise",
			Content:   "no signal at all",
			Vector:    []float32{0, 1, 0},
			Timestamp: now,
		},
	})

	pipe := LTM(emb,
		WithMultiRecall(true),
		WithEntityExtractor(func(_ context.Context, _ string) ([]string, error) {
			return []string{"alice"}, nil
		}),
		WithLimit(5),
		WithScoreThreshold(0), // disable threshold so the test is not at the mercy of tiny fused scores
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText: "favourite coffee",
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := make([]string, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		gotIDs = append(gotIDs, h.Doc.ID)
	}
	// Both signal docs must appear in the result set and rank above
	// the noise doc. We don't pin their internal order because RRF
	// can swap them depending on lane fan-out, and either ranking is
	// correct under the contract.
	pos := func(id string) int {
		for i, x := range gotIDs {
			if x == id {
				return i
			}
		}
		return -1
	}
	if pos("entity-hit") < 0 || pos("vector-hit") < 0 {
		t.Fatalf("expected both entity-hit and vector-hit in results, got %v", gotIDs)
	}
	if p := pos("noise"); p >= 0 {
		// Noise can appear via the BM25 lane (it shares no tokens
		// with the query, but the in-memory BM25 may still index it
		// at zero score). What matters is that it doesn't lead.
		if p < pos("entity-hit") || p < pos("vector-hit") {
			t.Fatalf("noise outranks signal docs: %v", gotIDs)
		}
	}
}

// TestLTMMultiRecall_NilEmbedderFallsBack asserts that passing
// WithMultiRecall when no embedder is configured does NOT emit a
// partial-multi-recall topology (which would be missing the vector
// lane and would behave worse than the legacy BM25-only path);
// instead the LTM gracefully falls back to the legacy single-lane
// recipe.
func TestLTMMultiRecall_NilEmbedderFallsBack(t *testing.T) {
	pipe := LTM(nil, WithMultiRecall(true))
	names := stageNames(pipe)
	if containsName(names, "MultiRetrieve") {
		t.Fatalf("multi-recall with nil embedder should fall back, got %v", names)
	}
	if !containsNamePrefix(names, "Retrieve(") {
		t.Fatalf("expected legacy single-lane Retrieve fallback, got %v", names)
	}
}

func stageNames(p *Pipeline) []string {
	out := make([]string, 0, len(p.stages))
	for _, s := range p.stages {
		out = append(out, s.Name())
	}
	return out
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func containsNamePrefix(names []string, prefix string) bool {
	for _, n := range names {
		if len(n) >= len(prefix) && n[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
