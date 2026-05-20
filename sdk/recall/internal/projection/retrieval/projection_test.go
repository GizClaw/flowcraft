package retrieval

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// stubEmbedder is a deterministic test embedder. It emits a fixed-size
// vector where each dimension is the count of a sentinel character in
// the input — enough to verify Project pipes Content through and that
// EmbedBatch returns one vector per input.
type stubEmbedder struct {
	dim   int
	calls int
}

func (s *stubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	s.calls++
	vec := make([]float32, s.dim)
	for i := 0; i < len(text) && i < s.dim; i++ {
		vec[i] = float32(text[i])
	}
	return vec, nil
}

func (s *stubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	s.calls++
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, s.dim)
		for j := 0; j < len(t) && j < s.dim; j++ {
			vec[j] = float32(t[j])
		}
		out[i] = vec
	}
	return out, nil
}

func TestProjection_WithEmbedder_PopulatesDocVector(t *testing.T) {
	idx := retrievalmem.New()
	emb := &stubEmbedder{dim: 8}
	p, err := New(idx, WithEmbedder(emb))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	now := time.Now()
	f := domain.TemporalFact{
		ID:         "f1",
		Scope:      scope,
		Kind:       domain.KindNote,
		Content:    "Alice met Bob",
		ObservedAt: now,
	}
	if err := p.Project(context.Background(), []domain.TemporalFact{f}); err != nil {
		t.Fatalf("project: %v", err)
	}
	got, ok, err := idx.Get(context.Background(), NamespaceFor(scope), "f1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if len(got.Vector) != emb.dim {
		t.Fatalf("expected Vector len %d, got %d", emb.dim, len(got.Vector))
	}
	if emb.calls == 0 {
		t.Fatal("expected embedder to be invoked")
	}
}

// failEmbedder returns an error on every call; the projection MUST
// degrade gracefully and still index the doc (BM25-only).
type failEmbedder struct{}

func (failEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errStub{}
}
func (failEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, errStub{}
}

type errStub struct{}

func (errStub) Error() string { return "stub embedder failure" }

// partialBatchEmbedder fails the batch path but succeeds per-text.
// Mirrors providers that occasionally drop batch requests under load
// (rate limits, schema rejection on a single item) while still
// serving per-text Embed.
type partialBatchEmbedder struct{ dim int }

func (p partialBatchEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, p.dim)
	for i := 0; i < len(text) && i < p.dim; i++ {
		vec[i] = float32(text[i])
	}
	return vec, nil
}

func (partialBatchEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, errStub{}
}

func TestProjection_WithEmbedder_FallsBackToPerTextOnBatchFailure(t *testing.T) {
	idx := retrievalmem.New()
	p, err := New(idx, WithEmbedder(partialBatchEmbedder{dim: 8}))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	if err := p.Project(context.Background(), []domain.TemporalFact{
		{ID: "a", Scope: scope, Kind: domain.KindNote, Content: "Alice met Bob"},
		{ID: "b", Scope: scope, Kind: domain.KindNote, Content: "Bob went to Paris"},
	}); err != nil {
		t.Fatalf("project: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		got, ok, err := idx.Get(context.Background(), NamespaceFor(scope), id)
		if err != nil || !ok {
			t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
		}
		if len(got.Vector) != 8 {
			t.Errorf("expected Vector len 8 from per-text fallback, got %d on %s", len(got.Vector), id)
		}
	}
}

func TestProjection_WithEmbedder_UsesContentNotSearchableText(t *testing.T) {
	// The vector lane must embed clean prose (f.Content), not the
	// concatenated BM25 buildContent output. We assert this by
	// configuring an embedder that records every input text and
	// checking that the recorded text equals Content verbatim — if
	// the projection had passed Doc.Content (= buildContent) we'd
	// see entities/evidence concatenated in.
	rec := &recordingEmbedder{dim: 4}
	p, err := New(retrievalmem.New(), WithEmbedder(rec))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	if err := p.Project(context.Background(), []domain.TemporalFact{{
		ID: "a", Scope: scope, Kind: domain.KindState,
		Content:      "Alice lives in Paris",
		Subject:      "alice",
		Predicate:    "city",
		Object:       "paris",
		Entities:     []string{"alice", "paris"},
		EvidenceText: "I'm in Paris these days",
	}}); err != nil {
		t.Fatalf("project: %v", err)
	}
	if len(rec.texts) != 1 {
		t.Fatalf("expected 1 embed input, got %d", len(rec.texts))
	}
	if rec.texts[0] != "Alice lives in Paris" {
		t.Errorf("expected vector lane to embed clean Content, got %q", rec.texts[0])
	}
}

type recordingEmbedder struct {
	dim   int
	texts []string
}

func (r *recordingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	r.texts = append(r.texts, text)
	return make([]float32, r.dim), nil
}

func (r *recordingEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	r.texts = append(r.texts, texts...)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, r.dim)
	}
	return out, nil
}

func TestProjection_WithEmbedder_DegradesOnFailure(t *testing.T) {
	idx := retrievalmem.New()
	p, err := New(idx, WithEmbedder(failEmbedder{}))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	f := domain.TemporalFact{
		ID:         "f1",
		Scope:      scope,
		Kind:       domain.KindNote,
		Content:    "Alice met Bob",
		ObservedAt: time.Now(),
	}
	if err := p.Project(context.Background(), []domain.TemporalFact{f}); err != nil {
		t.Fatalf("project must not propagate embedder failure: %v", err)
	}
	got, ok, err := idx.Get(context.Background(), NamespaceFor(scope), "f1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if len(got.Vector) != 0 {
		t.Fatalf("expected no Vector on embedder failure, got len %d", len(got.Vector))
	}
}

func TestProjection_UpsertsReservedMetadata(t *testing.T) {
	idx := retrievalmem.New()
	p, err := New(idx)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := domain.TemporalFact{
		ID:         "f1",
		Scope:      scope,
		Kind:       domain.KindState,
		Content:    "Alice lives in Paris",
		Subject:    "alice",
		Predicate:  "city",
		Object:     "paris",
		Entities:   []string{"alice"},
		MergeKey:   "state|alice|city",
		Confidence: 0.7,
		ObservedAt: validFrom,
		ValidFrom:  &validFrom,
		Metadata:   map[string]any{"user_key": "user_val"},
	}
	if err := p.Project(context.Background(), []domain.TemporalFact{f}); err != nil {
		t.Fatalf("project: %v", err)
	}

	got, ok, err := idx.Get(context.Background(), NamespaceFor(scope), "f1")
	if err != nil || !ok {
		t.Fatalf("expected doc upserted, ok=%v err=%v", ok, err)
	}
	if got.Content != "Alice lives in Paris alice city paris alice" {
		t.Errorf("content = %q", got.Content)
	}
	for key, want := range map[string]any{
		domain.MetaFactID:    "f1",
		domain.MetaFactKind:  string(domain.KindState),
		domain.MetaMergeKey:  "state|alice|city",
		domain.MetaScopeRT:   "rt",
		domain.MetaScopeUser: "u1",
		"user_key":          "user_val",
	} {
		if got.Metadata[key] != want {
			t.Errorf("meta[%q] = %v, want %v", key, got.Metadata[key], want)
		}
	}
	if got.Metadata[domain.MetaValidFrom].(int64) != validFrom.UnixMilli() {
		t.Errorf("valid_from metadata not in unix-millis: %v", got.Metadata[domain.MetaValidFrom])
	}
}

func TestProjection_SearchContentIncludesEvidenceGrounding(t *testing.T) {
	idx := retrievalmem.New()
	p, err := New(idx)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	f := domain.TemporalFact{
		ID:           "f1",
		Scope:        scope,
		Kind:         domain.KindEvent,
		Content:      "Caroline joined a support group",
		MergeKey:     "event|caroline|support",
		ObservedAt:   time.Unix(1, 0),
		EvidenceText: "[9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
		EvidenceRefs: []domain.EvidenceRef{{
			ID:   "D1:3",
			Text: "Caroline said the group met downtown on 7 May.",
		}},
	}
	if err := p.Project(context.Background(), []domain.TemporalFact{f}); err != nil {
		t.Fatalf("project: %v", err)
	}

	resp, err := idx.Search(context.Background(), NamespaceFor(scope), retrieval.SearchRequest{
		QueryText: "LGBTQ downtown 7 May",
		TopK:      5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "f1" {
		t.Fatalf("evidence grounding should be searchable, hits=%+v", resp.Hits)
	}
}

func TestProjection_UserMetaCannotOverrideReserved(t *testing.T) {
	idx := retrievalmem.New()
	p, _ := New(idx)
	scope := domain.Scope{RuntimeID: "rt"}
	f := domain.TemporalFact{
		ID:         "f1",
		Scope:      scope,
		Kind:       domain.KindNote,
		Content:    "x",
		MergeKey:   "k",
		ObservedAt: time.Unix(1, 0),
		Metadata: map[string]any{
			domain.MetaFactID:   "spoof",
			domain.MetaMergeKey: "spoof",
		},
	}
	if err := p.Project(context.Background(), []domain.TemporalFact{f}); err != nil {
		t.Fatalf("project: %v", err)
	}
	got, _, _ := idx.Get(context.Background(), NamespaceFor(scope), "f1")
	if got.Metadata[domain.MetaFactID] != "f1" {
		t.Errorf("user metadata leaked into reserved fact_id: %v", got.Metadata[domain.MetaFactID])
	}
	if got.Metadata[domain.MetaMergeKey] != "k" {
		t.Errorf("user metadata leaked into reserved merge_key: %v", got.Metadata[domain.MetaMergeKey])
	}
}

func TestProjection_ForgetRemovesDoc(t *testing.T) {
	idx := retrievalmem.New()
	p, _ := New(idx)
	scope := domain.Scope{RuntimeID: "rt"}
	f := domain.TemporalFact{
		ID:         "f1",
		Scope:      scope,
		Kind:       domain.KindNote,
		MergeKey:   "k",
		ObservedAt: time.Unix(1, 0),
	}
	_ = p.Project(context.Background(), []domain.TemporalFact{f})
	if err := p.Forget(context.Background(), scope, []string{"f1"}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	_, ok, _ := idx.Get(context.Background(), NamespaceFor(scope), "f1")
	if ok {
		t.Error("doc should be removed after Forget")
	}
}

func TestProjection_GroupsByScopeNamespace(t *testing.T) {
	idx := retrievalmem.New()
	p, _ := New(idx)
	scopeA := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	scopeB := domain.Scope{RuntimeID: "rt", UserID: "u2"}
	mk := func(id string, s domain.Scope) domain.TemporalFact {
		return domain.TemporalFact{ID: id, Scope: s, Kind: domain.KindNote, MergeKey: "k", ObservedAt: time.Unix(1, 0)}
	}
	err := p.Project(context.Background(), []domain.TemporalFact{mk("a", scopeA), mk("b", scopeB)})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scopeA), "a"); !ok {
		t.Error("a not in scopeA namespace")
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scopeB), "b"); !ok {
		t.Error("b not in scopeB namespace")
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scopeA), "b"); ok {
		t.Error("b leaked into scopeA namespace")
	}
}

func TestProjection_RebuildDropsStaleDocs(t *testing.T) {
	idx := retrievalmem.New()
	p, _ := New(idx)
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	fresh := domain.TemporalFact{
		ID:         "fresh",
		Scope:      scope,
		Kind:       domain.KindNote,
		Content:    "fresh",
		MergeKey:   "note|fresh",
		ObservedAt: time.Unix(1, 0),
	}
	stale := domain.TemporalFact{
		ID:         "stale",
		Scope:      scope,
		Kind:       domain.KindNote,
		Content:    "stale",
		MergeKey:   "note|stale",
		ObservedAt: time.Unix(1, 0),
	}
	if err := p.Project(context.Background(), []domain.TemporalFact{fresh, stale}); err != nil {
		t.Fatalf("initial project: %v", err)
	}
	if err := p.Rebuild(context.Background(), scope, []domain.TemporalFact{fresh}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scope), "fresh"); !ok {
		t.Fatal("fresh doc missing after rebuild")
	}
	if _, ok, _ := idx.Get(context.Background(), NamespaceFor(scope), "stale"); ok {
		t.Fatal("rebuild must remove docs not present in the supplied ledger snapshot")
	}
}

// compile-time guard: retrieval.Index has not regressed in shape.
var _ retrieval.Index = (*retrievalmem.Index)(nil)
