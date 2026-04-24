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

func TestLayerRepo_VectorModeReturnsEmpty(t *testing.T) {
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
