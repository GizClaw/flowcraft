package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/embedding"
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
		// Disable the entity-lane selectivity gate: this synthetic
		// corpus only has 3 docs, so "alice" can't be ">10% rare"
		// against itself. Real conversation namespaces can have
		// hundreds of docs and the gate's default 0.1 ratio is meaningful;
		// here we just want the lane to fire so we can verify
		// fusion picks up the entity hit.
		WithEntityLaneMinSelectivity(-1),
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

// TestEntityRecall_IDFPrefersRareAtoms is the regression test for the
// high-frequency-atom collapse: when the entity lane fired naively,
// every doc containing a high-frequency calendar atom ("tuesday")
// got the same overlap=1 score, drowning the few docs that also
// matched a rare proper noun ("lgbtq"). IDF weighting must rank the
// rare-atom doc strictly above docs that match only the common atom.
func TestEntityRecall_IDFPrefersRareAtoms(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()

	docs := make([]retrieval.Doc, 0, 25)
	// 20 facts each carry the common atom "tuesday" but nothing else
	// distinctive. Without IDF the entity lane would return any of
	// these (sorted only by timestamp) as a plausible top-1.
	for i := 0; i < 20; i++ {
		docs = append(docs, retrieval.Doc{
			ID:        "common-" + string(rune('a'+i)),
			Content:   "miscellaneous tuesday note",
			Metadata:  map[string]any{"entities": []any{"tuesday"}},
			Timestamp: now,
		})
	}
	// 1 fact carries the rare atom "lgbtq" alongside "tuesday".
	// IDF must lift this above the common-only docs.
	docs = append(docs, retrieval.Doc{
		ID:        "rare",
		Content:   "caroline joined the lgbtq group on tuesday",
		Metadata:  map[string]any{"entities": []any{"tuesday", "lgbtq", "caroline"}},
		Timestamp: now,
	})
	if err := idx.Upsert(ctx, ns, docs); err != nil {
		t.Fatal(err)
	}

	// Manually construct the state because we're testing the recall
	// stage in isolation, not the full LTM pipeline.
	st := &State{
		Request:       &retrieval.SearchRequest{TopK: 5},
		Index:         idx,
		Namespace:     ns,
		QueryEntities: []string{"tuesday", "lgbtq"},
	}
	hits, err := runEntityRecall(ctx, st, *st.Request, RetrieveSpec{Mode: ModeEntity, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected entity hits, got 0")
	}
	if hits[0].Doc.ID != "rare" {
		t.Fatalf("expected rare-atom doc first, got %v", hits[0].Doc.ID)
	}
	// The rare doc's score must exceed any common-only doc by at
	// least the IDF gap of "lgbtq" (which appears in 1/21 docs).
	rare := hits[0].Score
	for _, h := range hits[1:] {
		if h.Doc.ID == "rare" {
			continue
		}
		if h.Score >= rare {
			t.Fatalf("common-only doc %q tied or beat rare doc: %.3f vs %.3f",
				h.Doc.ID, h.Score, rare)
		}
	}
}

// TestEntityRecall_MinSelectivityGate is the regression test for the
// query-routing fix: when every query atom is
// "universal" (appears in >= MinSelectivity * N docs), the lane
// short-circuits to zero hits regardless of how many candidates the
// filter matched. This is the precision-protection mechanism that
// keeps queries dominated by calendar / common-noun atoms from
// flooding RRF with low-information candidates.
func TestEntityRecall_MinSelectivityGate(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()

	// 10 docs all carry "tuesday" (universal atom for this corpus).
	// 1 doc additionally carries "lgbtq" (rare atom).
	docs := make([]retrieval.Doc, 0, 11)
	for i := 0; i < 10; i++ {
		docs = append(docs, retrieval.Doc{
			ID:        "tue-" + string(rune('a'+i)),
			Metadata:  map[string]any{"entities": []any{"tuesday"}},
			Timestamp: now,
		})
	}
	docs = append(docs, retrieval.Doc{
		ID:        "rare",
		Metadata:  map[string]any{"entities": []any{"tuesday", "lgbtq"}},
		Timestamp: now,
	})
	if err := idx.Upsert(ctx, ns, docs); err != nil {
		t.Fatal(err)
	}

	t.Run("only-universal-atom-gated-out", func(t *testing.T) {
		st := &State{
			Request:       &retrieval.SearchRequest{TopK: 5},
			Index:         idx,
			Namespace:     ns,
			QueryEntities: []string{"tuesday"},
		}
		hits, err := runEntityRecall(ctx, st, *st.Request, RetrieveSpec{
			Mode: ModeEntity, TopK: 5, MinSelectivity: 0.5,
		})
		if err != nil {
			t.Fatal(err)
		}
		// "tuesday" df=11, threshold = floor(0.5*11) = 5; df 11 is
		// not strictly less than 5, no rare atom, lane skips.
		if len(hits) != 0 {
			t.Fatalf("expected gate to skip lane when only universal atoms in query, got %d hits", len(hits))
		}
	})

	t.Run("rare-atom-fires-lane", func(t *testing.T) {
		st := &State{
			Request:       &retrieval.SearchRequest{TopK: 5},
			Index:         idx,
			Namespace:     ns,
			QueryEntities: []string{"tuesday", "lgbtq"},
		}
		hits, err := runEntityRecall(ctx, st, *st.Request, RetrieveSpec{
			Mode: ModeEntity, TopK: 5, MinSelectivity: 0.5,
		})
		if err != nil {
			t.Fatal(err)
		}
		// "lgbtq" df=1, threshold = 5; df 1 < 5, lane fires and
		// must rank the rare doc first via IDF.
		if len(hits) == 0 || hits[0].Doc.ID != "rare" {
			t.Fatalf("expected lane to fire and rank rare doc first, got %+v", hits)
		}
	})
}

// TestEntityRecall_DropsZeroIDFMatches asserts that docs which only
// match query atoms appearing in every namespace doc are dropped
// from the lane — keeping them would let RRF rank-vote them in
// regardless, which is the pre-IDF failure mode.
func TestEntityRecall_DropsZeroIDFMatches(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()
	for i := 0; i < 5; i++ {
		_ = idx.Upsert(ctx, ns, []retrieval.Doc{{
			ID:        "only-tuesday-" + string(rune('a'+i)),
			Metadata:  map[string]any{"entities": []any{"tuesday"}},
			Timestamp: now,
		}})
	}
	st := &State{
		Request:       &retrieval.SearchRequest{TopK: 5},
		Index:         idx,
		Namespace:     ns,
		QueryEntities: []string{"tuesday"},
	}
	hits, err := runEntityRecall(ctx, st, *st.Request, RetrieveSpec{Mode: ModeEntity, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected zero hits when all candidates match only a corpus-universal atom, got %d", len(hits))
	}
}
