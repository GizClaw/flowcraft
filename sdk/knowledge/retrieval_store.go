package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// RetrievalStore is a Store implementation backed by a retrieval.Index
// ( Phase 2 swap-in).
//
// Responsibilities — derived from the legacy FSStore but delegated to the
// unified retrieval layer:
//
//   - Document persistence: one retrieval.Doc per chunk; document-level rows
//     (L0/L1) live in dedicated namespaces so Search at MaxLayer=L0/L1 only
//     scans the relevant tier.
//   - Hybrid search: pipeline.Knowledge composes BM25/vector/RRFFusion;
//     RetrievalStore does NOT contain its own ranking code (delegated).
//   - Layered context: Abstract/Overview/DatasetAbstract/DatasetOverview are
//     read by ID-prefix from a single namespace.
//
// Namespace layout (one retrieval namespace per dataset+layer):
//
//	kb_<dataset>__chunks   — one Doc per chunk; metadata { dataset, doc_name, chunk_index }
//	kb_<dataset>__docmeta  — one Doc per document for L0/L1; ID = "abstract:<doc>" / "overview:<doc>" / "doc:<doc>"
//	kb__datasets           — dataset-level summaries; ID = "abstract:<dataset>" / "overview:<dataset>"
//
// Caller responsibility: pre-compute embeddings for chunks (when supplying
// an Embedder) — RetrievalStore reuses GenericEmbedder if
// configured.
//
// Deprecated: use factory.NewRetrieval(docs, idx, opts...) which returns a
// *Service backed by backend/retrieval. Removed in v0.3.0.
type RetrievalStore struct {
	idx       retrieval.Index
	embedder  embedding.Embedder
	pipeline  *pipeline.Pipeline
	chunkCfg  ChunkConfig
	tokenizer Tokenizer
	now       func() time.Time
}

// RetrievalStoreOption configures a RetrievalStore.
type RetrievalStoreOption func(*RetrievalStore)

// WithRetrievalEmbedder sets the embedder used to vectorize chunks at write time.
func WithRetrievalEmbedder(e embedding.Embedder) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.embedder = e }
}

// WithRetrievalPipeline overrides the default pipeline.Knowledge(emb, nil).
func WithRetrievalPipeline(p *pipeline.Pipeline) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.pipeline = p }
}

// WithRetrievalChunkConfig overrides the default chunk config.
func WithRetrievalChunkConfig(c ChunkConfig) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.chunkCfg = c }
}

// WithRetrievalTokenizer overrides the BM25 tokenizer.
func WithRetrievalTokenizer(t Tokenizer) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.tokenizer = t }
}

// NewRetrievalStore wires a Store to a retrieval.Index. The store is safe
// for concurrent use; it does not own idx (caller must Close).
//
// Deprecated: use factory.NewRetrieval(docs, idx, opts...). Removed in v0.3.0.
func NewRetrievalStore(idx retrieval.Index, opts ...RetrievalStoreOption) *RetrievalStore {
	s := &RetrievalStore{
		idx:      idx,
		chunkCfg: DefaultChunkConfig(),
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.tokenizer == nil {
		s.tokenizer = DetectTokenizer("")
	}
	if s.pipeline == nil {
		s.pipeline = pipeline.Knowledge(s.embedder, nil)
	}
	return s
}

// Index exposes the underlying retrieval.Index for callers that need to
// drop down to a non-Store API (e.g. List or Iterate for reindex).
func (s *RetrievalStore) Index() retrieval.Index { return s.idx }

// chunkNS returns the retrieval namespace for chunks of dataset.
func chunkNS(dataset string) string   { return "kb_" + saneNS(dataset) + "__chunks" }
func docMetaNS(dataset string) string { return "kb_" + saneNS(dataset) + "__docmeta" }

const datasetMetaNS = "kb__datasets"

func saneNS(s string) string {
	if s == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// AddDocument implements Store.
func (s *RetrievalStore) AddDocument(ctx context.Context, datasetID, name, content string) error {
	body, meta := parseFrontmatter(content)
	if err := s.upsertDocChunks(ctx, datasetID, name, body, meta); err != nil {
		return err
	}
	return s.upsertDocMeta(ctx, datasetID, name, meta)
}

// AddDocuments implements Store.
func (s *RetrievalStore) AddDocuments(ctx context.Context, datasetID string, docs []DocInput) error {
	for _, d := range docs {
		if err := s.AddDocument(ctx, datasetID, d.Name, d.Content); err != nil {
			return fmt.Errorf("add %s: %w", d.Name, err)
		}
	}
	return nil
}

func (s *RetrievalStore) upsertDocChunks(ctx context.Context, datasetID, name, body string, meta map[string]string) error {
	chunks := ChunkDocument(name, body, s.chunkCfg)
	if len(chunks) == 0 {
		return nil
	}
	docs := make([]retrieval.Doc, 0, len(chunks))
	for _, c := range chunks {
		md := map[string]any{
			"dataset":     datasetID,
			"doc_name":    name,
			"chunk_index": c.Index,
			"layer":       string(LayerDetail),
		}
		for k, v := range meta {
			md["fm_"+k] = v
		}
		var vec []float32
		if s.embedder != nil {
			v, err := s.embedder.Embed(ctx, c.Content)
			if err != nil {
				return fmt.Errorf("embed chunk %d: %w", c.Index, err)
			}
			vec = v
		}
		docs = append(docs, retrieval.Doc{
			ID:        chunkID(datasetID, name, c.Index),
			Content:   c.Content,
			Vector:    vec,
			Metadata:  md,
			Timestamp: s.now().UTC(),
		})
	}
	return s.idx.Upsert(ctx, chunkNS(datasetID), docs)
}

func (s *RetrievalStore) upsertDocMeta(ctx context.Context, datasetID, name string, meta map[string]string) error {
	md := map[string]any{
		"dataset":  datasetID,
		"doc_name": name,
	}
	for k, v := range meta {
		md["fm_"+k] = v
	}
	doc := retrieval.Doc{
		ID:        "doc:" + name,
		Content:   name,
		Metadata:  md,
		Timestamp: s.now().UTC(),
	}
	return s.idx.Upsert(ctx, docMetaNS(datasetID), []retrieval.Doc{doc})
}

// SetAbstract / SetOverview let callers persist L0/L1 results without
// reusing the FSStore-specific sidecar mechanism.
func (s *RetrievalStore) SetAbstract(ctx context.Context, datasetID, name, abstract string) error {
	return s.upsertLayer(ctx, datasetID, name, LayerAbstract, abstract)
}

// SetOverview persists the L1 overview for a document.
func (s *RetrievalStore) SetOverview(ctx context.Context, datasetID, name, overview string) error {
	return s.upsertLayer(ctx, datasetID, name, LayerOverview, overview)
}

func (s *RetrievalStore) upsertLayer(ctx context.Context, datasetID, name string, layer ContextLayer, content string) error {
	md := map[string]any{"dataset": datasetID, "doc_name": name, "layer": string(layer)}
	var vec []float32
	if s.embedder != nil {
		v, err := s.embedder.Embed(ctx, content)
		if err != nil {
			return err
		}
		vec = v
	}
	return s.idx.Upsert(ctx, docMetaNS(datasetID), []retrieval.Doc{{
		ID: string(layer) + ":" + name, Content: content, Vector: vec,
		Metadata: md, Timestamp: s.now().UTC(),
	}})
}

// SetDatasetAbstract / SetDatasetOverview persist dataset-level summaries.
func (s *RetrievalStore) SetDatasetAbstract(ctx context.Context, datasetID, abstract string) error {
	return s.idx.Upsert(ctx, datasetMetaNS, []retrieval.Doc{{
		ID: "abstract:" + saneNS(datasetID), Content: abstract,
		Metadata:  map[string]any{"dataset": datasetID, "layer": string(LayerAbstract)},
		Timestamp: s.now().UTC(),
	}})
}

// SetDatasetOverview persists a dataset-level L1 overview.
func (s *RetrievalStore) SetDatasetOverview(ctx context.Context, datasetID, overview string) error {
	return s.idx.Upsert(ctx, datasetMetaNS, []retrieval.Doc{{
		ID: "overview:" + saneNS(datasetID), Content: overview,
		Metadata:  map[string]any{"dataset": datasetID, "layer": string(LayerOverview)},
		Timestamp: s.now().UTC(),
	}})
}

// GetDocument implements Store. It assembles the document body by listing
// all chunks for the requested doc_name in chunk-index order.
func (s *RetrievalStore) GetDocument(ctx context.Context, datasetID, name string) (*Document, error) {
	chunks, err := s.listChunksFor(ctx, datasetID, name)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	var body strings.Builder
	meta := map[string]string{}
	for i, c := range chunks {
		if i > 0 {
			body.WriteString("\n\n")
		}
		body.WriteString(c.Content)
		for k, v := range c.Metadata {
			if strings.HasPrefix(k, "fm_") {
				if sv, ok := v.(string); ok {
					meta[strings.TrimPrefix(k, "fm_")] = sv
				}
			}
		}
	}
	abstract, _ := s.Abstract(ctx, datasetID, name)
	overview, _ := s.Overview(ctx, datasetID, name)
	return &Document{Name: name, Content: body.String(), Abstract: abstract, Overview: overview, Metadata: meta}, nil
}

func (s *RetrievalStore) listChunksFor(ctx context.Context, datasetID, name string) ([]retrieval.Doc, error) {
	tok := ""
	var out []retrieval.Doc
	for {
		page, err := s.idx.List(ctx, chunkNS(datasetID), retrieval.ListRequest{
			Filter:    retrieval.Filter{Eq: map[string]any{"doc_name": name}},
			PageSize:  500,
			PageToken: tok,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			break
		}
		tok = page.NextPageToken
	}
	sort.SliceStable(out, func(i, j int) bool {
		return chunkIdx(out[i]) < chunkIdx(out[j])
	})
	return out, nil
}

func chunkIdx(d retrieval.Doc) int {
	if d.Metadata == nil {
		return 0
	}
	if v, ok := d.Metadata["chunk_index"]; ok {
		switch t := v.(type) {
		case int:
			return t
		case int64:
			return int(t)
		case float64:
			return int(t)
		}
	}
	return 0
}

// DeleteDocument implements Store.
func (s *RetrievalStore) DeleteDocument(ctx context.Context, datasetID, name string) error {
	chunks, err := s.listChunksFor(ctx, datasetID, name)
	if err != nil {
		return err
	}
	if len(chunks) > 0 {
		ids := make([]string, 0, len(chunks))
		for _, c := range chunks {
			ids = append(ids, c.ID)
		}
		if err := s.idx.Delete(ctx, chunkNS(datasetID), ids); err != nil {
			return err
		}
	}
	return s.idx.Delete(ctx, docMetaNS(datasetID), []string{
		"doc:" + name, string(LayerAbstract) + ":" + name, string(LayerOverview) + ":" + name,
	})
}

// ListDocuments implements Store.
func (s *RetrievalStore) ListDocuments(ctx context.Context, datasetID string) ([]Document, error) {
	tok := ""
	seen := map[string]struct{}{}
	var docs []Document
	for {
		page, err := s.idx.List(ctx, docMetaNS(datasetID), retrieval.ListRequest{
			PageSize:  500,
			PageToken: tok,
		})
		if err != nil {
			return nil, err
		}
		for _, d := range page.Items {
			if !strings.HasPrefix(d.ID, "doc:") {
				continue
			}
			name := strings.TrimPrefix(d.ID, "doc:")
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			docs = append(docs, Document{Name: name})
		}
		if page.NextPageToken == "" {
			break
		}
		tok = page.NextPageToken
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Name < docs[j].Name })
	return docs, nil
}

// Search implements Store via pipeline.Knowledge.
func (s *RetrievalStore) Search(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error) {
	topK := opts.TopK
	if topK <= 0 {
		topK = 10
	}
	threshold := opts.Threshold
	// NB: RRF scores are typically O(1/60..1/120) so the legacy
	// DefaultThreshold (0.1, BM25-scale) would filter every result.
	// Only apply when the caller explicitly opts in.
	ns := chunkNS(datasetID)
	if opts.MaxLayer != "" && opts.MaxLayer != LayerDetail {
		ns = docMetaNS(datasetID)
	}
	resp, err := s.pipeline.Run(ctx, s.idx, ns, retrieval.SearchRequest{
		QueryText: query,
		TopK:      topK,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		if h.Score < threshold {
			continue
		}
		layer := LayerDetail
		if v, ok := h.Doc.Metadata["layer"].(string); ok {
			layer = ContextLayer(v)
		}
		docName, _ := h.Doc.Metadata["doc_name"].(string)
		out = append(out, SearchResult{
			Content: h.Doc.Content, Score: h.Score, DocName: docName,
			ChunkIndex: chunkIdx(h.Doc), Layer: layer, Metadata: h.Doc.Metadata,
		})
	}
	return out, nil
}

// getByID fetches a single document by ID. Uses retrieval.DocGetter when
// the index supports it (zero round-trips), otherwise falls back to a
// filtered List scan over the namespace.
func (s *RetrievalStore) getByID(ctx context.Context, ns, id string) (retrieval.Doc, bool, error) {
	if g, ok := s.idx.(retrieval.DocGetter); ok {
		return g.Get(ctx, ns, id)
	}
	tok := ""
	for {
		page, err := s.idx.List(ctx, ns, retrieval.ListRequest{PageSize: 500, PageToken: tok})
		if err != nil {
			return retrieval.Doc{}, false, err
		}
		for _, d := range page.Items {
			if d.ID == id {
				return d, true, nil
			}
		}
		if page.NextPageToken == "" {
			return retrieval.Doc{}, false, nil
		}
		tok = page.NextPageToken
	}
}

// Abstract implements Store.
func (s *RetrievalStore) Abstract(ctx context.Context, datasetID, name string) (string, error) {
	d, ok, err := s.getByID(ctx, docMetaNS(datasetID), string(LayerAbstract)+":"+name)
	if err != nil || !ok {
		return "", err
	}
	return d.Content, nil
}

// Overview implements Store.
func (s *RetrievalStore) Overview(ctx context.Context, datasetID, name string) (string, error) {
	d, ok, err := s.getByID(ctx, docMetaNS(datasetID), string(LayerOverview)+":"+name)
	if err != nil || !ok {
		return "", err
	}
	return d.Content, nil
}

// DatasetAbstract implements Store.
func (s *RetrievalStore) DatasetAbstract(ctx context.Context, datasetID string) (string, error) {
	d, ok, err := s.getByID(ctx, datasetMetaNS, "abstract:"+saneNS(datasetID))
	if err != nil || !ok {
		return "", err
	}
	return d.Content, nil
}

// DatasetOverview implements Store.
func (s *RetrievalStore) DatasetOverview(ctx context.Context, datasetID string) (string, error) {
	d, ok, err := s.getByID(ctx, datasetMetaNS, "overview:"+saneNS(datasetID))
	if err != nil || !ok {
		return "", err
	}
	return d.Content, nil
}

func chunkID(datasetID, name string, idx int) string {
	return fmt.Sprintf("%s/%s#%d", datasetID, name, idx)
}
