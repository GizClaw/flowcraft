package knowledge_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/factory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// stubEmbedder is a deterministic embedder used by service_test to
// exercise vector lanes without pulling in a real provider.
type stubEmbedder struct{ dim int }

func (e *stubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if e.dim <= 0 {
		e.dim = 4
	}
	out := make([]float32, e.dim)
	for i := 0; i < len(text) && i < e.dim; i++ {
		out[i] = float32(text[i]) / 256
	}
	return out, nil
}

func (e *stubEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := e.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func newLocalService(t *testing.T, opts ...factory.LocalOption) *knowledge.Service {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	return factory.NewLocal(ws, opts...)
}

func TestService_PutDocumentIncrementsVersion(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := svc.PutDocument(ctx, "ds", "a.md", "beta"); err != nil {
		t.Fatalf("second put: %v", err)
	}
	doc, err := svc.GetDocument(ctx, "ds", "a.md")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if doc == nil || doc.Version != 2 {
		t.Fatalf("version = %d, want 2", doc.Version)
	}
	if doc.Content != "beta" {
		t.Fatalf("content = %q, want beta", doc.Content)
	}
}

func TestService_PutDocumentReplacesChunks(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha banana"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := svc.PutDocument(ctx, "ds", "a.md", "epsilon zeta"); err != nil {
		t.Fatalf("put 2: %v", err)
	}
	res, err := svc.Search(ctx, knowledge.Query{
		Scope: knowledge.ScopeAllDatasets,
		Text:  "epsilon",
		Mode:  knowledge.ModeBM25,
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	contents := map[string]bool{}
	for _, h := range res.Hits {
		contents[h.Content] = true
	}
	if contents["alpha banana"] {
		t.Fatalf("stale chunk surfaced after replace: %+v", res.Hits)
	}
}

func TestService_DeleteDocumentRemovesChunks(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := svc.DeleteDocument(ctx, "ds", "a.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	doc, err := svc.GetDocument(ctx, "ds", "a.md")
	if err == nil && doc != nil {
		t.Fatalf("doc should be gone, got %+v", doc)
	}
	res, err := svc.Search(ctx, knowledge.Query{
		Scope: knowledge.ScopeAllDatasets,
		Text:  "alpha",
		Mode:  knowledge.ModeBM25,
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, h := range res.Hits {
		if h.DocName == "a.md" {
			t.Fatalf("chunk for deleted doc surfaced: %+v", h)
		}
	}
}

func TestService_SearchSingleDatasetRequiresID(t *testing.T) {
	svc := newLocalService(t)
	_, err := svc.Search(context.Background(), knowledge.Query{
		Scope: knowledge.ScopeSingleDataset,
		Text:  "x",
	})
	if err == nil || !strings.Contains(err.Error(), "dataset_id") {
		t.Fatalf("expected dataset_id validation error, got %v", err)
	}
}

func TestService_SearchAllDatasetsFanOut(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds1", "a.md", "shared apple"); err != nil {
		t.Fatalf("put ds1: %v", err)
	}
	if err := svc.PutDocument(ctx, "ds2", "b.md", "shared banana"); err != nil {
		t.Fatalf("put ds2: %v", err)
	}
	res, err := svc.Search(ctx, knowledge.Query{
		Scope: knowledge.ScopeAllDatasets,
		Text:  "shared",
		Mode:  knowledge.ModeBM25,
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	seen := map[string]bool{}
	for _, h := range res.Hits {
		seen[h.DatasetID] = true
	}
	if !seen["ds1"] || !seen["ds2"] {
		t.Fatalf("fan-out missed a dataset: seen=%v", seen)
	}
}

func TestService_SearchInvalidLayer(t *testing.T) {
	svc := newLocalService(t)
	_, err := svc.Search(context.Background(), knowledge.Query{
		Scope: knowledge.ScopeAllDatasets,
		Layer: knowledge.Layer("L99"),
		Text:  "x",
	})
	if err == nil {
		t.Fatalf("expected validation error for invalid layer")
	}
}

func TestService_PutDocumentLayerStoresBoth(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("put doc: %v", err)
	}
	if err := svc.PutDocumentLayer(ctx, "ds", "a.md", knowledge.LayerAbstract, "abstract body"); err != nil {
		t.Fatalf("put abstract: %v", err)
	}
	got, err := svc.Layer(ctx, "ds", "a.md", knowledge.LayerAbstract)
	if err != nil {
		t.Fatalf("get layer: %v", err)
	}
	if got != "abstract body" {
		t.Fatalf("layer = %q, want %q", got, "abstract body")
	}
}

func TestService_PutDatasetLayer(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDatasetLayer(context.Background(), "ds", knowledge.LayerOverview, "ds overview"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := svc.DatasetLayer(ctx, "ds", knowledge.LayerOverview)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "ds overview" {
		t.Fatalf("got %q", got)
	}
}

func TestService_PutLayerRejectsLayerDetail(t *testing.T) {
	svc := newLocalService(t)
	err := svc.PutDocumentLayer(context.Background(), "ds", "a.md", knowledge.LayerDetail, "x")
	if err == nil {
		t.Fatalf("expected validation error for LayerDetail")
	}
}

func TestService_RebuildScopedDocument(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := svc.Rebuild(ctx, knowledge.RebuildScope{DatasetID: "ds", DocName: "a.md"}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	res, err := svc.Search(ctx, knowledge.Query{
		Scope: knowledge.ScopeSingleDataset, DatasetID: "ds",
		Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("rebuild dropped chunks")
	}
}

func TestService_RebuildWholeDataset(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := svc.Rebuild(ctx, knowledge.RebuildScope{DatasetID: "ds"}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
}

func TestService_VectorEnabledStampsEmbedSig(t *testing.T) {
	emb := &stubEmbedder{dim: 4}
	svc := newLocalService(t,
		factory.WithLocalEmbedder(emb, "stub:v1"),
	)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("put: %v", err)
	}
	res, err := svc.Search(ctx, knowledge.Query{
		Scope: knowledge.ScopeAllDatasets,
		Text:  "alpha",
		Mode:  knowledge.ModeVector,
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("vector search returned nothing")
	}
	if res.Hits[0].Sig.EmbedSig != "stub:v1" {
		t.Fatalf("embed sig = %q, want stub:v1", res.Hits[0].Sig.EmbedSig)
	}
}

func TestService_HybridFallsBackToBM25WhenNoVector(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("put: %v", err)
	}
	res, err := svc.Search(ctx, knowledge.Query{
		Scope: knowledge.ScopeAllDatasets,
		Text:  "alpha",
		Mode:  knowledge.ModeHybrid,
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("hybrid degraded to nothing without an embedder")
	}
}

func TestService_NoDocsReturnsEmpty(t *testing.T) {
	svc := newLocalService(t)
	res, err := svc.Search(context.Background(), knowledge.Query{
		Scope: knowledge.ScopeAllDatasets,
		Text:  "anything",
		Mode:  knowledge.ModeBM25,
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("expected no hits, got %+v", res.Hits)
	}
}

func TestService_DeleteUnknownDocumentNoError(t *testing.T) {
	svc := newLocalService(t)
	if err := svc.DeleteDocument(context.Background(), "ds", "missing.md"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

// errEmbedder is used to ensure embedder failures bubble out of PutDocument.
type errEmbedder struct{}

func (e *errEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("boom")
}
func (e *errEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("boom")
}

func TestService_PutDocumentSurfacesEmbedderError(t *testing.T) {
	svc := newLocalService(t,
		factory.WithLocalEmbedder(&errEmbedder{}, "err:v1"),
	)
	err := svc.PutDocument(context.Background(), "ds", "a.md", "alpha")
	if err == nil {
		t.Fatalf("expected embedder error to bubble up")
	}
}
