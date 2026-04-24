package fs

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func newLayerRepo(t *testing.T) (*FSLayerRepo, *workspace.MemWorkspace) {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	return NewLayerRepo(ws, "kb", &textsearch.SimpleTokenizer{}), ws
}

func TestLayerRepo_PutAndGetDocLayer(t *testing.T) {
	r, ws := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds",
		DocName:   "a.md",
		Layer:     knowledge.LayerAbstract,
		Content:   "abstract text",
		Sig:       knowledge.DerivedSig{SourceVer: 1, PromptSig: "p1"},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := r.Get(ctx, "ds", "a.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Content != "abstract text" || got.Sig.SourceVer != 1 || got.Sig.PromptSig != "p1" {
		t.Fatalf("get returned %+v", got)
	}
	exists, _ := ws.Exists(ctx, "kb/ds/a.abstract")
	if !exists {
		t.Fatalf("abstract sidecar missing on disk")
	}
}

func TestLayerRepo_PutAndGetDatasetLayer(t *testing.T) {
	r, ws := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds",
		Layer:     knowledge.LayerOverview,
		Content:   "dataset overview",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := r.Get(ctx, "ds", "", knowledge.LayerOverview)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Content != "dataset overview" {
		t.Fatalf("get returned %+v", got)
	}
	exists, _ := ws.Exists(ctx, "kb/ds/.overview.md")
	if !exists {
		t.Fatalf("dataset overview file missing on disk")
	}
}

func TestLayerRepo_RejectsLayerDetail(t *testing.T) {
	r, _ := newLayerRepo(t)
	err := r.Put(context.Background(), knowledge.DerivedLayer{
		DatasetID: "ds",
		DocName:   "a.md",
		Layer:     knowledge.LayerDetail,
		Content:   "x",
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("got %v, want Validation", err)
	}
}

func TestLayerRepo_GetMissingReturnsNil(t *testing.T) {
	r, _ := newLayerRepo(t)
	got, err := r.Get(context.Background(), "ds", "missing.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestLayerRepo_DeleteByDoc(t *testing.T) {
	r, ws := newLayerRepo(t)
	ctx := context.Background()
	for _, layer := range []knowledge.Layer{knowledge.LayerAbstract, knowledge.LayerOverview} {
		if err := r.Put(ctx, knowledge.DerivedLayer{DatasetID: "ds", DocName: "a.md", Layer: layer, Content: "x"}); err != nil {
			t.Fatalf("put %s: %v", layer, err)
		}
	}
	if err := r.DeleteByDoc(ctx, "ds", "a.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, p := range []string{"kb/ds/a.abstract", "kb/ds/a.overview", "kb/ds/a.layers.json"} {
		exists, _ := ws.Exists(ctx, p)
		if exists {
			t.Fatalf("path %s still exists", p)
		}
	}
}

func TestLayerRepo_DeleteByDataset(t *testing.T) {
	r, ws := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{DatasetID: "ds", Layer: knowledge.LayerAbstract, Content: "x"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := r.DeleteByDataset(ctx, "ds"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	exists, _ := ws.Exists(ctx, "kb/ds/.abstract.md")
	if exists {
		t.Fatalf("dataset abstract should be deleted")
	}
}

func TestLayerRepo_SearchHonoursLayer(t *testing.T) {
	r, _ := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds", DocName: "a.md", Layer: knowledge.LayerAbstract,
		Content: "alpha beta gamma",
	}); err != nil {
		t.Fatalf("put doc l0: %v", err)
	}
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds", DocName: "a.md", Layer: knowledge.LayerOverview,
		Content: "delta epsilon zeta",
	}); err != nil {
		t.Fatalf("put doc l1: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.LayerQuery{
		DatasetIDs: []string{"ds"},
		Layer:      knowledge.LayerAbstract,
		Text:       "alpha",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) != 1 || cands[0].Hit.Layer != knowledge.LayerAbstract {
		t.Fatalf("layer-strict search miss: %+v", cands)
	}

	cands, err = r.Search(ctx, knowledge.LayerQuery{
		DatasetIDs: []string{"ds"},
		Layer:      knowledge.LayerOverview,
		Text:       "epsilon",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search overview: %v", err)
	}
	if len(cands) != 1 || cands[0].Hit.Layer != knowledge.LayerOverview {
		t.Fatalf("overview search miss: %+v", cands)
	}
}

func TestLayerRepo_SearchIncludesDatasetLevel(t *testing.T) {
	r, _ := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds", Layer: knowledge.LayerAbstract,
		Content: "dataset summary keyword apple",
	}); err != nil {
		t.Fatalf("put dataset l0: %v", err)
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
	if len(cands) != 1 || cands[0].Hit.DocName != "" {
		t.Fatalf("dataset layer not surfaced: %+v", cands)
	}
}

// When no vector sidecar is present the vector lane has nothing to
// score, so ModeVector must safely return an empty slice rather than
// erroring out.
func TestLayerRepo_VectorModeWithoutSidecarIsEmpty(t *testing.T) {
	r, _ := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds", DocName: "a.md", Layer: knowledge.LayerAbstract,
		Content: "x",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.LayerQuery{
		DatasetIDs: []string{"ds"},
		Layer:      knowledge.LayerAbstract,
		Mode:       knowledge.ModeVector,
		Vector:     []float32{1, 0},
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search vector: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("vector search should be empty, got %+v", cands)
	}
}

// PutAndGet must round-trip the embedding via the .vec sidecar so the
// engine can later score it.
func TestLayerRepo_PutPersistsVectorSidecar(t *testing.T) {
	r, ws := newLayerRepo(t)
	ctx := context.Background()
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds",
		DocName:   "a.md",
		Layer:     knowledge.LayerAbstract,
		Content:   "with embedding",
		Vector:    vec,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	exists, _ := ws.Exists(ctx, "kb/ds/a.abstract.vec")
	if !exists {
		t.Fatalf("vec sidecar missing on disk")
	}
	got, err := r.Get(ctx, "ds", "a.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || len(got.Vector) != len(vec) {
		t.Fatalf("vector not roundtripped: %+v", got)
	}
	for i := range vec {
		if got.Vector[i] != vec[i] {
			t.Fatalf("vector[%d] = %v, want %v", i, got.Vector[i], vec[i])
		}
	}
}

// Re-Putting with an empty Vector must drop a previously persisted
// sidecar so vector and text never drift apart.
func TestLayerRepo_PutEmptyVectorRemovesSidecar(t *testing.T) {
	r, ws := newLayerRepo(t)
	ctx := context.Background()
	base := knowledge.DerivedLayer{
		DatasetID: "ds", DocName: "a.md", Layer: knowledge.LayerAbstract,
		Content: "v1", Vector: []float32{1, 2, 3},
	}
	if err := r.Put(ctx, base); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if exists, _ := ws.Exists(ctx, "kb/ds/a.abstract.vec"); !exists {
		t.Fatalf("vec sidecar should exist after first put")
	}
	base.Content = "v2"
	base.Vector = nil
	if err := r.Put(ctx, base); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	if exists, _ := ws.Exists(ctx, "kb/ds/a.abstract.vec"); exists {
		t.Fatalf("vec sidecar should be removed when re-put with empty vector")
	}
}

// ModeVector must rank by cosine similarity and ignore the BM25 lane.
func TestLayerRepo_SearchVectorMode(t *testing.T) {
	r, _ := newLayerRepo(t)
	ctx := context.Background()
	put := func(doc string, vec []float32) {
		t.Helper()
		if err := r.Put(ctx, knowledge.DerivedLayer{
			DatasetID: "ds", DocName: doc, Layer: knowledge.LayerAbstract,
			Content: doc + " content", Vector: vec,
		}); err != nil {
			t.Fatalf("put %s: %v", doc, err)
		}
	}
	put("near.md", []float32{1, 0.1, 0})
	put("far.md", []float32{0.2, 1, 0})

	cands, err := r.Search(ctx, knowledge.LayerQuery{
		DatasetIDs: []string{"ds"},
		Layer:      knowledge.LayerAbstract,
		Mode:       knowledge.ModeVector,
		Vector:     []float32{1, 0, 0},
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 cands, got %d (%+v)", len(cands), cands)
	}
	if cands[0].Hit.DocName != "near" {
		t.Fatalf("expected near first, got %+v", cands)
	}
	if cands[0].Hit.Score <= cands[1].Hit.Score {
		t.Fatalf("near should outrank far: %+v", cands)
	}
}

// ModeHybrid runs both lanes and surfaces a doc that only matches via
// vector similarity (BM25 keyword would miss it).
func TestLayerRepo_SearchHybridFusesLanes(t *testing.T) {
	r, _ := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds", DocName: "vec_only.md", Layer: knowledge.LayerAbstract,
		Content: "totally unrelated text", Vector: []float32{1, 0, 0},
	}); err != nil {
		t.Fatalf("put vec_only: %v", err)
	}
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds", DocName: "bm25_only.md", Layer: knowledge.LayerAbstract,
		Content: "keyword apple banana",
	}); err != nil {
		t.Fatalf("put bm25_only: %v", err)
	}

	cands, err := r.Search(ctx, knowledge.LayerQuery{
		DatasetIDs: []string{"ds"},
		Layer:      knowledge.LayerAbstract,
		Mode:       knowledge.ModeHybrid,
		Text:       "apple",
		Vector:     []float32{1, 0, 0},
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search hybrid: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected both lanes, got %d (%+v)", len(cands), cands)
	}
	docs := map[string]bool{}
	for _, c := range cands {
		docs[c.Hit.DocName] = true
	}
	if !docs["vec_only"] || !docs["bm25_only"] {
		t.Fatalf("hybrid missed a lane: %+v", cands)
	}
}

// DeleteByDoc must wipe both layer text and the .vec sidecar.
func TestLayerRepo_DeleteByDocRemovesVectorSidecar(t *testing.T) {
	r, ws := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds", DocName: "a.md", Layer: knowledge.LayerAbstract,
		Content: "x", Vector: []float32{1, 2, 3},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := r.DeleteByDoc(ctx, "ds", "a.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, p := range []string{"kb/ds/a.abstract", "kb/ds/a.abstract.vec"} {
		if exists, _ := ws.Exists(ctx, p); exists {
			t.Fatalf("path %s should be deleted", p)
		}
	}
}

// DeleteByDataset must wipe dataset-level layer text and the .vec sidecar.
func TestLayerRepo_DeleteByDatasetRemovesVectorSidecar(t *testing.T) {
	r, ws := newLayerRepo(t)
	ctx := context.Background()
	if err := r.Put(ctx, knowledge.DerivedLayer{
		DatasetID: "ds", Layer: knowledge.LayerAbstract,
		Content: "ds-level", Vector: []float32{1, 2, 3},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := r.DeleteByDataset(ctx, "ds"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, p := range []string{"kb/ds/.abstract.md", "kb/ds/.abstract.md.vec"} {
		if exists, _ := ws.Exists(ctx, p); exists {
			t.Fatalf("path %s should be deleted", p)
		}
	}
}
