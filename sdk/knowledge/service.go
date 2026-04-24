package knowledge

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Service orchestrates document lifecycle, derived-data persistence and
// search. All public Knowledge entry points (graph node, tools,
// deprecated stores) route through Service so contract guarantees
// (#1..#7 in doc.go) live in one place.
type Service struct {
	docs   DocumentRepo
	chunks ChunkRepo
	layers LayerRepo
	engine *SearchEngine

	chunker  Chunker
	embedder Embedder
	embedSig string
	now      func() time.Time
}

// ServiceOptions configures a Service. Nil-friendly: every field is
// optional and falls back to a sensible default.
type ServiceOptions struct {
	// Chunker overrides the default chunker; nil means "use
	// NewDefaultChunker(DefaultChunkConfig())".
	Chunker Chunker
	// Embedder enables vector indexing and semantic search; nil
	// disables vector lanes.
	Embedder Embedder
	// EmbedSig is stamped onto every DerivedSig produced while this
	// Service runs. Embedder doesn't expose a model identifier, so
	// callers (typically the Factory) supply one. Empty string means
	// "use the embedder's Go type name", which is good enough for
	// freshness checks within a single binary but not stable across
	// processes — production wiring should set it explicitly.
	EmbedSig string
	// Now overrides the clock; nil means time.Now (unit-test hook).
	Now func() time.Time
}

// NewService constructs a Service from explicit repositories.
//
// Most callers should use NewLocalService / NewRetrievalService instead;
// this entry point exists so out-of-tree backends can be wired the same
// way the built-ins are.
func NewService(
	docs DocumentRepo,
	chunks ChunkRepo,
	layers LayerRepo,
	engine *SearchEngine,
	opts ServiceOptions,
) *Service {
	chunker := opts.Chunker
	if chunker == nil {
		chunker = NewDefaultChunker(DefaultChunkConfig())
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	embedSig := opts.EmbedSig
	if embedSig == "" && opts.Embedder != nil {
		embedSig = fmt.Sprintf("embedder:%T", opts.Embedder)
	}
	return &Service{
		docs:     docs,
		chunks:   chunks,
		layers:   layers,
		engine:   engine,
		chunker:  chunker,
		embedder: opts.Embedder,
		embedSig: embedSig,
		now:      now,
	}
}

// --- Document lifecycle ----------------------------------------------------

// PutDocument writes raw content under (datasetID, name).
//
// Behaviour:
//   - SourceDocument.Version is incremented atomically by DocumentRepo.
//   - DerivedChunks are recomputed and ChunkRepo.Replace is called.
//   - DerivedLayers are NOT touched (layer generation is explicit; see
//     PutDocumentLayer / PutDatasetLayer).
//
// Atomicity model: chunk replacement happens AFTER the document write
// succeeds. A failure between the two leaves the document persisted but
// chunks stale; the next PutDocument or Rebuild restores consistency.
// Backends with native transactions can override this by composing
// repos that share a transaction.
func (s *Service) PutDocument(ctx context.Context, datasetID, name, raw string) error {
	if s == nil || s.docs == nil {
		return errdefs.Validationf("knowledge: service has no document store")
	}
	if datasetID == "" || name == "" {
		return errdefs.Validationf("knowledge: dataset_id and name are required")
	}
	prev, err := s.docs.Get(ctx, datasetID, name)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	version := uint64(1)
	if prev != nil {
		version = prev.Version + 1
	}
	doc := SourceDocument{
		DatasetID: datasetID,
		Name:      name,
		Content:   raw,
		Version:   version,
		UpdatedAt: s.now(),
	}
	if prev != nil {
		doc.Metadata = copyStringMetadata(prev.Metadata)
	}
	if err := s.docs.Put(ctx, doc); err != nil {
		return err
	}
	return s.replaceChunks(ctx, doc)
}

// DeleteDocument removes the document and all its derived data
// (chunks + layers). Errors from chunk/layer cleanup are returned
// after a best-effort attempt so a single failure does not leave the
// document orphaned in DocumentRepo.
func (s *Service) DeleteDocument(ctx context.Context, datasetID, name string) error {
	if s == nil || s.docs == nil {
		return errdefs.Validationf("knowledge: service has no document store")
	}
	if datasetID == "" || name == "" {
		return errdefs.Validationf("knowledge: dataset_id and name are required")
	}
	if err := s.docs.Delete(ctx, datasetID, name); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if s.chunks != nil {
		if err := s.chunks.DeleteByDoc(ctx, datasetID, name); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	if s.layers != nil {
		if err := s.layers.DeleteByDoc(ctx, datasetID, name); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// GetDocument returns the lossless SourceDocument (contract #4).
func (s *Service) GetDocument(ctx context.Context, datasetID, name string) (*SourceDocument, error) {
	if s == nil || s.docs == nil {
		return nil, errdefs.Validationf("knowledge: service has no document store")
	}
	return s.docs.Get(ctx, datasetID, name)
}

// ListDocuments returns SourceDocuments in the dataset. Implementations
// MAY omit Content for performance; FSDocumentRepo currently returns it.
func (s *Service) ListDocuments(ctx context.Context, datasetID string) ([]SourceDocument, error) {
	if s == nil || s.docs == nil {
		return nil, errdefs.Validationf("knowledge: service has no document store")
	}
	return s.docs.List(ctx, datasetID)
}

// ListDatasets enumerates every known dataset id.
func (s *Service) ListDatasets(ctx context.Context) ([]string, error) {
	if s == nil || s.docs == nil {
		return nil, errdefs.Validationf("knowledge: service has no document store")
	}
	return s.docs.ListDatasets(ctx)
}

// --- Derived layers --------------------------------------------------------

// PutDocumentLayer persists an LLM-derived layer for one document.
// Caller is expected to have produced content via GenerateDocumentContext.
func (s *Service) PutDocumentLayer(ctx context.Context, datasetID, name string, layer Layer, content string) error {
	if s == nil || s.layers == nil {
		return errdefs.Validationf("knowledge: service has no layer store")
	}
	if datasetID == "" || name == "" {
		return errdefs.Validationf("knowledge: dataset_id and name are required")
	}
	return s.putLayer(ctx, datasetID, name, layer, content)
}

// PutDatasetLayer persists a dataset-level layer (DocName == "").
func (s *Service) PutDatasetLayer(ctx context.Context, datasetID string, layer Layer, content string) error {
	if s == nil || s.layers == nil {
		return errdefs.Validationf("knowledge: service has no layer store")
	}
	if datasetID == "" {
		return errdefs.Validationf("knowledge: dataset_id is required")
	}
	return s.putLayer(ctx, datasetID, "", layer, content)
}

// Layer reads a document-level layer; returns "" without error when
// missing (contract: callers should treat absence as "not yet generated").
func (s *Service) Layer(ctx context.Context, datasetID, name string, layer Layer) (string, error) {
	if s == nil || s.layers == nil {
		return "", nil
	}
	got, err := s.layers.Get(ctx, datasetID, name, layer)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if got == nil {
		return "", nil
	}
	return got.Content, nil
}

// DatasetLayer reads a dataset-level layer; returns "" without error
// when missing.
func (s *Service) DatasetLayer(ctx context.Context, datasetID string, layer Layer) (string, error) {
	return s.Layer(ctx, datasetID, "", layer)
}

// --- Search ----------------------------------------------------------------

// Search executes the query through the configured SearchEngine.
//
// Validation (the only place these checks live, contract #2):
//   - q.Layer defaults to LayerDetail when zero; rejected otherwise.
//   - q.Mode  defaults to ModeBM25 when zero; ModeSemantic is normalised
//     to ModeVector for backwards compatibility.
//   - q.Scope=ScopeSingleDataset requires q.DatasetID to be non-empty.
//
// For ScopeAllDatasets the dataset list is resolved once via
// DocumentRepo.ListDatasets and pushed down to retrievers via the
// unexported resolvedDatasets field.
func (s *Service) Search(ctx context.Context, q Query) (*Result, error) {
	if s == nil || s.engine == nil {
		return &Result{}, nil
	}
	if q.Layer == "" {
		q.Layer = LayerDetail
	}
	if !IsValidLayer(q.Layer) {
		return nil, errdefs.Validationf("knowledge: invalid layer %q", q.Layer)
	}
	q.Mode = ResolveMode(q.Mode)
	if !IsValidMode(q.Mode) {
		return nil, errdefs.Validationf("knowledge: invalid mode %q", q.Mode)
	}
	if q.Scope == ScopeSingleDataset && q.DatasetID == "" {
		return nil, errdefs.Validationf("knowledge: dataset_id is required for ScopeSingleDataset")
	}
	if q.TopK <= 0 {
		q.TopK = 5
	}

	ids, err := s.resolveDatasetIDs(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return &Result{}, nil
	}
	return s.engine.Search(ctx, q.withDatasets(ids))
}

// --- Rebuild ---------------------------------------------------------------

// Rebuild re-derives chunks for the requested scope, comparing
// DerivedSig against the current (SourceVer, ChunkerSig). Stale chunks
// are recomputed; up-to-date chunks are left alone.
//
// Layers are not regenerated automatically; callers drive layer
// rebuilds explicitly via PutDocumentLayer / PutDatasetLayer.
func (s *Service) Rebuild(ctx context.Context, scope RebuildScope) error {
	if s == nil || s.docs == nil {
		return errdefs.Validationf("knowledge: service has no document store")
	}
	datasets, err := s.rebuildDatasets(ctx, scope)
	if err != nil {
		return err
	}
	for _, ds := range datasets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		docs, err := s.rebuildDocuments(ctx, ds, scope)
		if err != nil {
			return err
		}
		for _, d := range docs {
			if err := s.replaceChunks(ctx, d); err != nil {
				return fmt.Errorf("knowledge: rebuild %s/%s: %w", ds, d.Name, err)
			}
		}
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func (s *Service) replaceChunks(ctx context.Context, doc SourceDocument) error {
	if s.chunks == nil {
		return nil
	}
	specs := s.chunker.Split(doc.Content)
	chunks := make([]DerivedChunk, len(specs))
	sig := DerivedSig{
		SourceVer:  doc.Version,
		ChunkerSig: s.chunker.Sig(),
	}
	if s.embedder != nil {
		sig.EmbedSig = s.embedSig
	}
	for i, sp := range specs {
		c := DerivedChunk{
			DatasetID: doc.DatasetID,
			DocName:   doc.Name,
			Index:     sp.Index,
			Offset:    sp.Offset,
			Content:   sp.Content,
			Sig:       sig,
		}
		if s.embedder != nil {
			vec, err := s.embedder.Embed(ctx, sp.Content)
			if err != nil {
				return fmt.Errorf("knowledge: embed chunk %d: %w", sp.Index, err)
			}
			c.Vector = vec
		}
		chunks[i] = c
	}
	return s.chunks.Replace(ctx, doc.DatasetID, doc.Name, chunks)
}

func (s *Service) putLayer(ctx context.Context, datasetID, docName string, layer Layer, content string) error {
	if !IsValidLayer(layer) {
		return errdefs.Validationf("knowledge: invalid layer %q", layer)
	}
	if layer == LayerDetail {
		return errdefs.Validationf("knowledge: LayerDetail is owned by chunks, not layers")
	}
	source, err := s.layerSource(ctx, datasetID, docName)
	if err != nil {
		return err
	}
	sig := DerivedSig{
		SourceVer: source,
		PromptSig: layerPromptSig(layer),
	}
	out := DerivedLayer{
		DatasetID: datasetID,
		DocName:   docName,
		Layer:     layer,
		Content:   content,
		Sig:       sig,
	}
	if s.embedder != nil && content != "" {
		vec, err := s.embedder.Embed(ctx, content)
		if err != nil {
			return fmt.Errorf("knowledge: embed layer: %w", err)
		}
		out.Vector = vec
		out.Sig.EmbedSig = s.embedSig
	}
	return s.layers.Put(ctx, out)
}

// layerSource returns the SourceVer to stamp on a DerivedLayer:
// document-level layers borrow the source's Version; dataset-level
// layers use 0 (no single source).
func (s *Service) layerSource(ctx context.Context, datasetID, docName string) (uint64, error) {
	if docName == "" || s.docs == nil {
		return 0, nil
	}
	doc, err := s.docs.Get(ctx, datasetID, docName)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	if doc == nil {
		return 0, nil
	}
	return doc.Version, nil
}

// resolveDatasetIDs collapses Query.Scope into a concrete fan-out list.
func (s *Service) resolveDatasetIDs(ctx context.Context, q Query) ([]string, error) {
	if q.Scope == ScopeSingleDataset {
		return []string{q.DatasetID}, nil
	}
	if s.docs == nil {
		return nil, nil
	}
	ids, err := s.docs.ListDatasets(ctx)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// rebuildDatasets honours scope.DatasetID; returns every dataset when
// the scope is open.
func (s *Service) rebuildDatasets(ctx context.Context, scope RebuildScope) ([]string, error) {
	if scope.DatasetID != "" {
		return []string{scope.DatasetID}, nil
	}
	return s.docs.ListDatasets(ctx)
}

func (s *Service) rebuildDocuments(ctx context.Context, datasetID string, scope RebuildScope) ([]SourceDocument, error) {
	if scope.DocName != "" {
		doc, err := s.docs.Get(ctx, datasetID, scope.DocName)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		if doc == nil {
			return nil, nil
		}
		return []SourceDocument{*doc}, nil
	}
	return s.docs.List(ctx, datasetID)
}

// layerPromptSig stamps a deterministic prompt identifier onto layers.
// Until the prompt set itself is configurable, this is just the layer
// constant; once GeneratorOptions exists we will hash the prompt body.
func layerPromptSig(layer Layer) string {
	return "prompt:" + string(layer)
}

func copyStringMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Compile-time guarantee that Service implements Rebuilder so it can
// be wired straight into NewReloader / NewEventReloader.
var _ Rebuilder = (*Service)(nil)
