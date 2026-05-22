package retrieval

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func newLayerRepo(t *testing.T) *RetrievalLayerRepo {
	t.Helper()
	return NewLayerRepo(memory.New())
}

func absLayer(dataset, doc, body string) knowledge.DerivedLayer {
	return knowledge.DerivedLayer{
		DatasetID: dataset,
		DocName:   doc,
		Layer:     knowledge.LayerAbstract,
		Content:   body,
		Sig:       knowledge.DerivedSig{SourceVer: 3, PromptSig: "p:v1"},
	}
}

func ovLayer(dataset, body string) knowledge.DerivedLayer {
	return knowledge.DerivedLayer{
		DatasetID: dataset,
		DocName:   "",
		Layer:     knowledge.LayerOverview,
		Content:   body,
		Sig:       knowledge.DerivedSig{SourceVer: 1, PromptSig: "p:ov"},
	}
}

func TestRetrievalLayerRepo_PutGetRoundtrip(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	in := absLayer("ds", "doc.md", "abstract body")
	if err := r.Put(ctx, in); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := r.Get(ctx, "ds", "doc.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatalf("get returned nil")
	}
	if got.Content != "abstract body" || got.Sig.SourceVer != 3 || got.Sig.PromptSig != "p:v1" {
		t.Fatalf("get mismatch: %+v", got)
	}
}

func TestRetrievalLayerRepo_GetMissingReturnsNil(t *testing.T) {
	r := newLayerRepo(t)
	got, err := r.Get(context.Background(), "ds", "missing.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing layer, got %+v", got)
	}
}

func TestRetrievalLayerRepo_RejectsUnsupportedLayer(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds",
		DocName:   "x.md",
		Layer:     knowledge.LayerDetail,
		Content:   "x",
	})
	if err == nil {
		t.Fatalf("expected validation error for LayerDetail")
	}
	_, err = r.Get(ctx, "ds", "x.md", knowledge.LayerDetail)
	if err == nil {
		t.Fatalf("expected validation error for LayerDetail in Get")
	}
}

func TestRetrievalLayerRepo_DatasetLevelOverview(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, ovLayer("ds", "dataset overview")); err != nil {
		t.Fatalf("put overview: %v", err)
	}
	got, err := r.Get(ctx, "ds", "", knowledge.LayerOverview)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Content != "dataset overview" {
		t.Fatalf("get overview: %+v", got)
	}
}

func TestRetrievalLayerRepo_DeleteByDoc(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, absLayer("ds", "a.md", "alpha")); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if err := r.Put(ctx, absLayer("ds", "b.md", "beta")); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if err := r.DeleteByDoc(ctx, "ds", "a.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := r.Get(ctx, "ds", "a.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("a.md still present: %+v", got)
	}
	got, err = r.Get(ctx, "ds", "b.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if got == nil {
		t.Fatalf("b.md should still exist")
	}
}

func TestRetrievalLayerRepo_DeleteByDataset(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, absLayer("ds", "a.md", "alpha")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := r.DeleteByDataset(ctx, "ds"); err != nil {
		t.Fatalf("delete dataset: %v", err)
	}
	got, err := r.Get(ctx, "ds", "a.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("dataset should be empty after drop: %+v", got)
	}
}

func TestRetrievalLayerRepo_SearchFiltersByLayer(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, absLayer("ds", "a.md", "abstract apple")); err != nil {
		t.Fatalf("put abstract: %v", err)
	}
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds",
		DocName:   "a.md",
		Layer:     knowledge.LayerOverview,
		Content:   "overview apple",
		Sig:       knowledge.DerivedSig{SourceVer: 1, PromptSig: "p:ov"},
	}); err != nil {
		t.Fatalf("put overview: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.LayerQuery{
		DatasetIDs: []string{"ds"},
		Layer:      knowledge.LayerAbstract,
		Text:       "apple",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, c := range cands {
		if c.Hit.Layer != knowledge.LayerAbstract {
			t.Fatalf("layer leak: %+v", c.Hit)
		}
	}
}

func TestRetrievalLayerRepo_SearchFanOut(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, absLayer("ds1", "a.md", "shared apple")); err != nil {
		t.Fatalf("put ds1: %v", err)
	}
	if err := r.Put(ctx, absLayer("ds2", "b.md", "shared banana")); err != nil {
		t.Fatalf("put ds2: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.LayerQuery{
		DatasetIDs: []string{"ds1", "ds2"},
		Layer:      knowledge.LayerAbstract,
		Text:       "shared",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range cands {
		seen[c.Hit.DatasetID] = true
	}
	if !seen["ds1"] || !seen["ds2"] {
		t.Fatalf("fan-out missed a dataset: seen=%v", seen)
	}
}

func TestRetrievalLayerRepo_VectorSearch(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	la := absLayer("ds", "a.md", "abstract a")
	la.Vector = []float32{1, 0, 0}
	lb := absLayer("ds", "b.md", "abstract b")
	lb.Vector = []float32{0, 1, 0}
	if err := r.Put(ctx, la); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if err := r.Put(ctx, lb); err != nil {
		t.Fatalf("put b: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.LayerQuery{
		DatasetIDs: []string{"ds"},
		Layer:      knowledge.LayerAbstract,
		Vector:     []float32{1, 0, 0},
		Mode:       knowledge.ModeVector,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) == 0 {
		t.Fatalf("vector search returned nothing")
	}
	if cands[0].Hit.DocName != "a.md" {
		t.Fatalf("top vector hit = %s, want a.md", cands[0].Hit.DocName)
	}
}

func TestRetrievalLayerRepo_EmptyDatasetIDsReturnsNil(t *testing.T) {
	r := newLayerRepo(t)
	cands, err := r.Search(context.Background(), knowledge.LayerQuery{Layer: knowledge.LayerAbstract, Text: "x", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("empty datasets must return nothing, got %+v", cands)
	}
}

func TestRetrievalLayerRepo_NamespaceIsolation(t *testing.T) {
	r := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, absLayer("ds1", "a.md", "alpha")); err != nil {
		t.Fatalf("put ds1: %v", err)
	}
	if err := r.Put(ctx, absLayer("ds2", "a.md", "alpha")); err != nil {
		t.Fatalf("put ds2: %v", err)
	}
	if err := r.DeleteByDataset(ctx, "ds1"); err != nil {
		t.Fatalf("delete ds1: %v", err)
	}
	got, err := r.Get(ctx, "ds2", "a.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get ds2: %v", err)
	}
	if got == nil {
		t.Fatalf("ds2 lost data after dropping ds1")
	}
}
