package knowledge_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/knowledge"
	"github.com/GizClaw/flowcraft/memory/knowledge/backend/fs"
	kretrieval "github.com/GizClaw/flowcraft/memory/knowledge/backend/retrieval"
	"github.com/GizClaw/flowcraft/memory/knowledge/factory"
	"github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type listWithoutContentRepo struct {
	knowledge.DocumentRepo
}

func (r *listWithoutContentRepo) List(ctx context.Context, datasetID string) ([]knowledge.SourceDocument, error) {
	docs, err := r.DocumentRepo.List(ctx, datasetID)
	if err != nil {
		return nil, err
	}
	for i := range docs {
		docs[i].Content = ""
	}
	return docs, nil
}

func TestService_RebuildFetchesContentAfterList(t *testing.T) {
	ctx := context.Background()
	base := fs.NewDocumentRepo(workspace.NewMemWorkspace(), fs.DefaultPrefix)
	if _, err := base.Put(ctx, knowledge.SourceDocument{DatasetID: "ds", Name: "a.md", Content: "alpha body"}); err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	docs := &listWithoutContentRepo{DocumentRepo: base}
	idx := memory.New()
	chunks := kretrieval.NewChunkRepo(idx)
	layers := kretrieval.NewLayerRepo(idx)
	engine := knowledge.NewSearchEngine([]knowledge.Retriever{knowledge.NewBM25Retriever(chunks)}, nil, nil)
	svc := knowledge.NewService(docs, chunks, layers, engine, knowledge.ServiceOptions{})

	if err := svc.Rebuild(ctx, knowledge.RebuildScope{DatasetID: "ds"}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	res, err := svc.Search(ctx, knowledge.Query{
		Scope: knowledge.ScopeSingleDataset, DatasetID: "ds",
		Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) == 0 || res.Hits[0].Content == "" {
		t.Fatalf("rebuild used List's empty Content instead of Get: %+v", res.Hits)
	}
}

type blockingDocRepo struct {
	mu      sync.Mutex
	doc     knowledge.SourceDocument
	block   bool
	started chan struct{}
	release chan struct{}
}

func newBlockingDocRepo() *blockingDocRepo {
	return &blockingDocRepo{
		doc: knowledge.SourceDocument{
			DatasetID: "ds",
			Name:      "a.md",
			Content:   "old apple",
			Version:   1,
		},
	}
}

func (r *blockingDocRepo) armNextGet() (<-chan struct{}, chan<- struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.block = true
	r.started = make(chan struct{})
	r.release = make(chan struct{})
	return r.started, r.release
}

func (r *blockingDocRepo) Put(_ context.Context, doc knowledge.SourceDocument) (*knowledge.SourceDocument, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doc.Content = doc.Content
	r.doc.Metadata = doc.Metadata
	r.doc.Version++
	return cloneSourceDocument(r.doc), nil
}

func (r *blockingDocRepo) Get(_ context.Context, datasetID, name string) (*knowledge.SourceDocument, error) {
	r.mu.Lock()
	if datasetID != r.doc.DatasetID || name != r.doc.Name {
		r.mu.Unlock()
		return nil, errdefs.NotFoundf("test doc %s/%s", datasetID, name)
	}
	doc := r.doc
	var started chan struct{}
	var release chan struct{}
	if r.block {
		r.block = false
		started = r.started
		release = r.release
	}
	r.mu.Unlock()
	if started != nil {
		close(started)
		<-release
	}
	return cloneSourceDocument(doc), nil
}

func (r *blockingDocRepo) Delete(_ context.Context, datasetID, name string) error {
	return nil
}

func (r *blockingDocRepo) List(_ context.Context, datasetID string) ([]knowledge.SourceDocument, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if datasetID != r.doc.DatasetID {
		return nil, nil
	}
	return []knowledge.SourceDocument{{DatasetID: r.doc.DatasetID, Name: r.doc.Name}}, nil
}

func (r *blockingDocRepo) ListDatasets(context.Context) ([]string, error) {
	return []string{"ds"}, nil
}

func cloneSourceDocument(doc knowledge.SourceDocument) *knowledge.SourceDocument {
	cp := doc
	if len(doc.Metadata) > 0 {
		cp.Metadata = make(map[string]string, len(doc.Metadata))
		for k, v := range doc.Metadata {
			cp.Metadata[k] = v
		}
	}
	return &cp
}

type sigChunkRepo struct {
	mu       sync.Mutex
	chunks   []knowledge.DerivedChunk
	replaced []uint64
}

func newSigChunkRepo() *sigChunkRepo {
	return &sigChunkRepo{
		chunks: []knowledge.DerivedChunk{{
			DatasetID: "ds",
			DocName:   "a.md",
			Index:     0,
			Content:   "old apple",
			Sig:       knowledge.DerivedSig{SourceVer: 1},
		}},
	}
}

func (r *sigChunkRepo) Replace(_ context.Context, datasetID, docName string, chunks []knowledge.DerivedChunk) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ver uint64
	if len(chunks) > 0 {
		ver = chunks[0].Sig.SourceVer
	}
	r.replaced = append(r.replaced, ver)
	r.chunks = append([]knowledge.DerivedChunk(nil), chunks...)
	return nil
}

func (r *sigChunkRepo) GetDocSig(_ context.Context, datasetID, docName string) (knowledge.DerivedSig, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.chunks {
		if c.DatasetID == datasetID && c.DocName == docName {
			return c.Sig, true, nil
		}
	}
	return knowledge.DerivedSig{}, false, nil
}

func (r *sigChunkRepo) DeleteByDoc(context.Context, string, string) error { return nil }
func (r *sigChunkRepo) DeleteByDataset(context.Context, string) error     { return nil }
func (r *sigChunkRepo) Search(context.Context, knowledge.ChunkQuery) ([]knowledge.Candidate, error) {
	return nil, nil
}

func TestService_ConcurrentPutBeatsStaleRebuild(t *testing.T) {
	ctx := context.Background()
	docs := newBlockingDocRepo()
	chunks := newSigChunkRepo()
	svc := knowledge.NewService(docs, chunks, nil, nil, knowledge.ServiceOptions{})

	rebuildGetStarted, releaseRebuildGet := docs.armNextGet()
	rebuildDone := make(chan error, 1)
	go func() {
		rebuildDone <- svc.Rebuild(ctx, knowledge.RebuildScope{DatasetID: "ds", DocName: "a.md"})
	}()
	select {
	case <-rebuildGetStarted:
	case <-time.After(time.Second):
		t.Fatalf("rebuild did not reach blocked Get")
	}

	if err := svc.PutDocument(ctx, "ds", "a.md", "new banana"); err != nil {
		t.Fatalf("concurrent put: %v", err)
	}
	close(releaseRebuildGet)
	select {
	case err := <-rebuildDone:
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("rebuild did not finish")
	}

	chunks.mu.Lock()
	defer chunks.mu.Unlock()
	if len(chunks.chunks) == 0 || chunks.chunks[0].Sig.SourceVer != 2 || chunks.chunks[0].Content != "new banana" {
		t.Fatalf("stale rebuild overwrote newer chunks: %+v", chunks.chunks)
	}
	for _, ver := range chunks.replaced {
		if ver == 1 {
			t.Fatalf("stale rebuild performed Replace with source version 1: %v", chunks.replaced)
		}
	}
}

func TestService_PutDocumentLayerMissingSourceReturnsNotFound(t *testing.T) {
	svc := newService(t)
	err := svc.PutDocumentLayer(context.Background(), "ds", "missing.md", knowledge.LayerAbstract, "abstract")
	if !errdefs.IsNotFound(err) {
		t.Fatalf("got %v, want NotFound", err)
	}
}

type failSecondEmbedder struct {
	inner stubEmbedder
	calls int
}

func (e *failSecondEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	e.calls++
	if e.calls >= 2 {
		return nil, errors.New("embed failed")
	}
	return e.inner.Embed(ctx, text)
}

func (e *failSecondEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vec, err := e.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
		out[i] = vec
	}
	return out, nil
}

func TestService_PutDocumentEmbedFailureKeepsOldChunks(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, factory.WithRetrievalEmbedder(&failSecondEmbedder{inner: stubEmbedder{dim: 4}}, "flaky:v1"))
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha"); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := svc.PutDocument(ctx, "ds", "a.md", "beta"); err == nil {
		t.Fatalf("expected second put to fail during embedding")
	}
	res, err := svc.Search(ctx, knowledge.Query{
		Scope: knowledge.ScopeSingleDataset, DatasetID: "ds",
		Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) == 0 || res.Hits[0].Content != "alpha" {
		t.Fatalf("old chunks were not preserved after embed failure: %+v", res.Hits)
	}
}
