// Package knowledge — v0.2.x compatibility layer (removed in v0.3.0).
//
// This file is the single home for every symbol that the v0.3.0
// architecture supersedes. Each symbol carries a // Deprecated: tag so
// staticcheck (SA1019) flags new callers; the index below is the
// canonical "what replaces what" map.
//
// === Replacement index ===
//
//	Storage / orchestration
//	  Store                    -> *Service                      (sdk/knowledge)
//	  FSStore                  -> factory.NewLocal              (sdk/knowledge/factory)
//	  RetrievalStore           -> factory.NewRetrieval          (sdk/knowledge/factory)
//	  CachedStore              -> (none — fold caching into the repo)
//
//	Data models
//	  Document                 -> SourceDocument + DerivedLayer
//	  SearchResult             -> Hit
//	  SearchOptions            -> Query (with Scope/Mode/Layer)
//	  Chunk                    -> DerivedChunk
//	  ContextLayer             -> Layer        (alias kept for transition)
//	  SearchMode               -> Mode         (alias kept for transition)
//	  ModeSemantic             -> ModeVector
//
//	Graph node
//	  KnowledgeConfig          -> KnowledgeNodeConfig
//	  KnowledgeNode            -> KnowledgeServiceNode
//	  NewKnowledgeNode         -> NewKnowledgeServiceNode
//	  KnowledgeConfigFromMap   -> KnowledgeNodeConfigFromMap
//	  RegisterNode             -> RegisterServiceNode
//	  KnowledgeNodeSchema      -> KnowledgeServiceNodeSchema
//
//	LLM tools
//	  NewSearchTool            -> NewSearchServiceTool
//	  NewAddTool               -> NewPutServiceTool
//
//	Reload pipeline
//	  ChangeNotifier           -> EventNotifier  (typed ChangeEvent stream)
//	  Reloader                 -> EventReloader  (scope-aware, serialised)
//	  NewReloader              -> NewEventReloader
//
//	Helpers
//	  ChunkDocument            -> ChunkText      (returns DerivedChunk)
//	  RankResults              -> RRFRanker (the SearchEngine.Ranker)
//	  RRFMerge                 -> RRFRanker
//	  ScoreChunk               -> textsearch.BM25 directly
//	  parseFrontmatter         -> Service handles frontmatter internally
//
// === Behaviour bridges that survive v0.3.0 ===
//
//	ResolveMode("")           -> ModeBM25
//	ResolveMode("semantic")   -> ModeVector
//	KnowledgeNodeConfigFromMap reads "max_layer" as "layer" when
//	  "layer" is absent.
//
// === Things that are NOT deprecated ===
//
//	GenerateDocumentContext / GenerateDatasetContext — the L0/L1
//	  derivation helpers remain external to Service so callers control
//	  scheduling, retry and persistence policy.
//	DatasetQuery — shared by knowledgenode.Config and the legacy
//	  KnowledgeConfig (lives in this file because both consumers reach it
//	  through the knowledge package).
//	Tokenizer / textsearch.Tokenizer — backend-neutral utility.
//	CosineSimilarity — used by backend implementations.
package knowledge

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

// =============================================================================
// Shared dataset query descriptor
// =============================================================================

// DatasetQuery describes a single dataset search within a Knowledge node.
// Re-used by both the v0.3.0 knowledgenode.Config and the deprecated
// KnowledgeConfig (kept stable across versions).
type DatasetQuery struct {
	DatasetID string `json:"dataset_id"`
	StateKey  string `json:"state_key"`
	TopK      int    `json:"top_k"`
}

// =============================================================================
// Data models
// =============================================================================

// Document represents a knowledge base document.
//
// Deprecated: use SourceDocument (raw content + Version) and DerivedLayer
// (L0/L1) separately. Document conflates the two and is removed in v0.3.0.
type Document struct {
	Name     string            `json:"name"`
	Content  string            `json:"content"`
	Abstract string            `json:"abstract,omitempty"` // L0
	Overview string            `json:"overview,omitempty"` // L1
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SearchResult represents a single search hit with its relevance score.
//
// Deprecated: use Hit. SearchResult is removed in v0.3.0.
type SearchResult struct {
	Content    string         `json:"content"`
	Score      float64        `json:"score"`
	DocName    string         `json:"doc_name,omitempty"`
	ChunkIndex int            `json:"chunk_index,omitempty"`
	Layer      ContextLayer   `json:"layer"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// SearchOptions configures a knowledge search query.
//
// Deprecated: use Query. The MaxLayer→Layer rename and ScopeAllDatasets
// fan-out live on Query. SearchOptions is removed in v0.3.0.
type SearchOptions struct {
	TopK      int          `json:"top_k,omitempty"`
	MaxLayer  ContextLayer `json:"max_layer,omitempty"`
	Threshold float64      `json:"threshold,omitempty"`
	Mode      SearchMode   `json:"mode,omitempty"`
}

// Chunk represents a segment of a document.
//
// Deprecated: use DerivedChunk. Chunk is removed in v0.3.0.
type Chunk struct {
	DocName string `json:"doc_name"`
	Index   int    `json:"index"`
	Content string `json:"content"`
	Offset  int    `json:"offset"`
}

// =============================================================================
// Store interface + DocInput
// =============================================================================

// DocInput is a name+content pair for batch document ingestion.
//
// Deprecated: use Service.PutDocument (one call per document).
// Removed in v0.3.0.
type DocInput struct {
	Name    string
	Content string
}

// Store abstracts knowledge base storage. Documents are organized by dataset.
//
// Deprecated: use *Service in sdk/knowledge instead. Service unifies
// document, chunk and layer storage behind a single contract and is the
// only orchestrator going forward; Store will be removed in v0.3.0.
//
// Migration:
//   - Replace AddDocument / AddDocuments with Service.PutDocument.
//   - Replace Search                          with Service.Search.
//   - Replace Abstract / Overview             with Service.Layer.
//   - Replace DatasetAbstract / Overview      with Service.DatasetLayer.
type Store interface {
	AddDocument(ctx context.Context, datasetID, name, content string) error
	AddDocuments(ctx context.Context, datasetID string, docs []DocInput) error
	GetDocument(ctx context.Context, datasetID, name string) (*Document, error)
	DeleteDocument(ctx context.Context, datasetID, name string) error
	ListDocuments(ctx context.Context, datasetID string) ([]Document, error)
	Search(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error)

	Abstract(ctx context.Context, datasetID, name string) (string, error)
	Overview(ctx context.Context, datasetID, name string) (string, error)

	DatasetAbstract(ctx context.Context, datasetID string) (string, error)
	DatasetOverview(ctx context.Context, datasetID string) (string, error)
}

// =============================================================================
// Legacy chunker (returns Chunk; see chunking.go for the v0.3.0 ChunkText)
// =============================================================================

// ChunkDocument splits content into overlapping chunks, preferring to
// break at paragraph or sentence boundaries.
//
// Deprecated: use ChunkText (returns []DerivedChunk). Removed in v0.3.0.
func ChunkDocument(docName, content string, cfg ChunkConfig) []Chunk {
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 512
	}
	if cfg.ChunkOverlap < 0 {
		cfg.ChunkOverlap = 0
	}
	if cfg.ChunkOverlap >= cfg.ChunkSize {
		cfg.ChunkOverlap = cfg.ChunkSize / 4
	}

	content = strings.TrimSpace(content)
	if len(content) == 0 {
		return nil
	}
	if len(content) <= cfg.ChunkSize {
		return []Chunk{{DocName: docName, Index: 0, Content: content, Offset: 0}}
	}

	var chunks []Chunk
	step := cfg.ChunkSize - cfg.ChunkOverlap
	if step <= 0 {
		step = 1
	}

	for offset := 0; offset < len(content); {
		end := offset + cfg.ChunkSize
		if end > len(content) {
			end = len(content)
		}

		if end < len(content) {
			if bp := legacyFindBreak(content[offset:end], "\n\n"); bp > 0 {
				end = offset + bp
			} else if bp := legacyFindBreak(content[offset:end], ". "); bp > 0 {
				end = offset + bp + 1
			} else if bp := legacyFindBreak(content[offset:end], "\n"); bp > 0 {
				end = offset + bp
			}
		}

		chunk := strings.TrimSpace(content[offset:end])
		if chunk != "" {
			chunks = append(chunks, Chunk{
				DocName: docName,
				Index:   len(chunks),
				Content: chunk,
				Offset:  offset,
			})
		}

		nextOffset := offset + (end - offset)
		if nextOffset <= offset {
			nextOffset = offset + step
		}
		nextOffset -= cfg.ChunkOverlap
		if nextOffset <= offset {
			nextOffset = offset + 1
		}
		if nextOffset >= len(content) {
			break
		}
		offset = nextOffset
	}
	return chunks
}

// legacyFindBreak is the deprecated counterpart of the v0.3.0 chunker's
// boundary search. Renamed to avoid colliding with chunking.go's findBreak.
func legacyFindBreak(s, sep string) int {
	minPos := len(s) * 3 / 4
	if minPos < len(s)/2 {
		minPos = len(s) / 2
	}
	idx := strings.LastIndex(s[minPos:], sep)
	if idx < 0 {
		return -1
	}
	return minPos + idx
}

// =============================================================================
// Legacy ranking helpers
// =============================================================================

// ScoreChunk computes the BM25 score for a chunk against query keywords.
//
// Deprecated: use textsearch.BM25 directly with DerivedChunk content.
// Removed in v0.3.0.
func ScoreChunk(chunk *Chunk, keywords []string, corpus *CorpusStats, tokenizer Tokenizer) float64 {
	if corpus == nil || corpus.DocCount == 0 || len(keywords) == 0 {
		return 0
	}
	tokens := tokenizer.Tokenize(chunk.Content)
	return textsearch.BM25(tokens, keywords, corpus)
}

// RankResults sorts by score descending and limits to topK.
//
// Deprecated: use the SearchEngine's Ranker (RRFRanker by default).
// Removed in v0.3.0.
func RankResults(results []SearchResult, topK int) []SearchResult {
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results
}

// rrfKey is the deduplication key for RRFMerge.
func rrfKey(r SearchResult) string {
	return fmt.Sprintf("%s|%d", r.DocName, r.ChunkIndex)
}

// RRFMerge fuses two ranked SearchResult lists with reciprocal-rank fusion.
//
// Deprecated: use RRFRanker. Removed in v0.3.0.
func RRFMerge(bm25Results, semanticResults []SearchResult, k int) []SearchResult {
	if k <= 0 {
		k = 60
	}
	type scored struct {
		result SearchResult
		rrf    float64
	}
	merged := make(map[string]*scored)
	for rank, r := range bm25Results {
		key := rrfKey(r)
		if s, ok := merged[key]; ok {
			s.rrf += 1.0 / float64(rank+k)
		} else {
			merged[key] = &scored{result: r, rrf: 1.0 / float64(rank+k)}
		}
	}
	for rank, r := range semanticResults {
		key := rrfKey(r)
		if s, ok := merged[key]; ok {
			s.rrf += 1.0 / float64(rank+k)
		} else {
			merged[key] = &scored{result: r, rrf: 1.0 / float64(rank+k)}
		}
	}
	results := make([]SearchResult, 0, len(merged))
	for _, s := range merged {
		s.result.Score = s.rrf
		results = append(results, s.result)
	}
	return results
}

// parseFrontmatter extracts YAML frontmatter (between "---" delimiters).
//
// Deprecated: Service / DocumentRepo handle frontmatter internally; this
// helper exists only for the legacy FSStore / RetrievalStore code paths.
// Removed in v0.3.0.
func parseFrontmatter(raw string) (body string, meta map[string]string) {
	if !strings.HasPrefix(raw, "---\n") {
		return raw, nil
	}
	end := strings.Index(raw[4:], "\n---")
	if end < 0 {
		return raw, nil
	}
	fmBlock := raw[4 : 4+end]
	body = strings.TrimLeft(raw[4+end+4:], "\n")
	meta = make(map[string]string)
	for _, line := range strings.Split(fmBlock, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			meta[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return body, meta
}

func legacyIsMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown" || ext == ".txt"
}

// =============================================================================
// FSStore (filesystem-backed Store implementation)
// =============================================================================

type posting struct {
	chunk *Chunk
	tf    int
	dl    int
}

type datasetIndex struct {
	chunks        []*Chunk
	docs          map[string]*Document
	stats         *CorpusStats
	abstractStats *CorpusStats
	abstract      string
	overview      string
	inverted      map[string][]posting
	vectors       map[*Chunk][]float32
}

func (di *datasetIndex) addChunkToInverted(chunk *Chunk, tokens []string) {
	tf := make(map[string]int, len(tokens))
	for _, t := range tokens {
		tf[t]++
	}
	dl := len(tokens)
	for term, freq := range tf {
		di.inverted[term] = append(di.inverted[term], posting{chunk: chunk, tf: freq, dl: dl})
	}
}

func (di *datasetIndex) removeFromInverted(docName string) {
	for term, postings := range di.inverted {
		var kept []posting
		for _, p := range postings {
			if p.chunk.DocName != docName {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			delete(di.inverted, term)
		} else {
			di.inverted[term] = kept
		}
	}
}

func (di *datasetIndex) searchL2(keywords []string, threshold float64) []SearchResult {
	if len(di.inverted) == 0 {
		return nil
	}
	type chunkScore struct {
		chunk *Chunk
		score float64
	}
	scores := make(map[*Chunk]*chunkScore)
	k1 := 1.2
	b := 0.75
	avgDL := di.stats.AvgLength
	if avgDL <= 0 {
		avgDL = 1
	}
	for _, kw := range keywords {
		postings, ok := di.inverted[kw]
		if !ok {
			continue
		}
		df := di.stats.DocFreq[kw]
		n := float64(di.stats.DocCount)
		idf := math.Log((n-float64(df)+0.5)/(float64(df)+0.5) + 1.0)
		for _, p := range postings {
			dl := float64(p.dl)
			tfNorm := float64(p.tf) * (k1 + 1) / (float64(p.tf) + k1*(1-b+b*dl/avgDL))
			cs, ok := scores[p.chunk]
			if !ok {
				cs = &chunkScore{chunk: p.chunk}
				scores[p.chunk] = cs
			}
			cs.score += idf * tfNorm
		}
	}
	var results []SearchResult
	for _, cs := range scores {
		if cs.score >= threshold {
			results = append(results, SearchResult{
				Content:    cs.chunk.Content,
				Score:      cs.score,
				DocName:    cs.chunk.DocName,
				ChunkIndex: cs.chunk.Index,
				Layer:      LayerDetail,
			})
		}
	}
	return results
}

// FSStoreOption configures an FSStore.
//
// Deprecated: see FSStore.
type FSStoreOption func(*FSStore)

// WithTokenizer sets the tokenizer for search and indexing.
//
// Deprecated: pass tokenizer through factory.WithLocalTokenizer.
func WithTokenizer(t Tokenizer) FSStoreOption {
	return func(s *FSStore) { s.tokenizer = t }
}

// WithChunkConfig sets the chunking configuration.
//
// Deprecated: configure ChunkConfig on factory.NewLocal's chunker option.
func WithChunkConfig(cfg ChunkConfig) FSStoreOption {
	return func(s *FSStore) { s.chunkCfg = cfg }
}

// WithEmbedder sets the embedder for semantic/hybrid search.
//
// Deprecated: pass embedder through factory.WithLocalEmbedder.
func WithEmbedder(e Embedder) FSStoreOption {
	return func(s *FSStore) { s.embedder = e }
}

// FSStore implements Store using a Workspace-backed file tree.
//
// Deprecated: use factory.NewLocal(ws, opts...). Removed in v0.3.0.
type FSStore struct {
	ws        workspace.Workspace
	prefix    string
	mu        sync.RWMutex
	index     map[string]*datasetIndex
	tokenizer Tokenizer
	chunkCfg  ChunkConfig
	embedder  Embedder
}

// NewFSStore creates a knowledge store rooted at the given prefix.
//
// Deprecated: use factory.NewLocal(ws, opts...). Removed in v0.3.0.
func NewFSStore(ws workspace.Workspace, opts ...FSStoreOption) *FSStore {
	s := &FSStore{
		ws:       ws,
		prefix:   "knowledge",
		index:    make(map[string]*datasetIndex),
		chunkCfg: DefaultChunkConfig(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.tokenizer == nil {
		s.tokenizer = &CJKTokenizer{}
	}
	return s
}

// BuildIndex scans all datasets and builds the in-memory search index.
func (s *FSStore) BuildIndex(ctx context.Context) error {
	entries, err := s.ws.List(ctx, s.prefix)
	if err != nil {
		return fmt.Errorf("knowledge: build index: %w", err)
	}
	idx := make(map[string]*datasetIndex)
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dsID := entry.Name()
		di, err := s.buildDatasetIndex(ctx, dsID)
		if err != nil {
			telemetry.Warn(ctx, "knowledge: failed to index dataset",
				otellog.String("dataset", dsID), otellog.String("error", err.Error()))
			errs = append(errs, fmt.Errorf("dataset %q: %w", dsID, err))
			continue
		}
		idx[dsID] = di
	}
	s.mu.Lock()
	s.index = idx
	s.mu.Unlock()
	return errors.Join(errs...)
}

func (s *FSStore) buildDatasetIndex(ctx context.Context, datasetID string) (*datasetIndex, error) {
	docs, err := s.listDocsFromDisk(ctx, datasetID)
	if err != nil {
		return nil, err
	}
	di := &datasetIndex{
		docs:          make(map[string]*Document, len(docs)),
		stats:         NewCorpusStats(),
		abstractStats: NewCorpusStats(),
		inverted:      make(map[string][]posting),
		vectors:       make(map[*Chunk][]float32),
	}
	for i := range docs {
		doc := &docs[i]
		di.docs[doc.Name] = doc
		doc.Abstract, _ = s.readSidecar(ctx, datasetID, doc.Name, ".abstract")
		doc.Overview, _ = s.readSidecar(ctx, datasetID, doc.Name, ".overview")
		chunks := ChunkDocument(doc.Name, doc.Content, s.chunkCfg)
		for j := range chunks {
			c := &chunks[j]
			tokens := s.tokenizer.Tokenize(c.Content)
			di.chunks = append(di.chunks, c)
			di.stats.AddDocument(tokens)
			di.addChunkToInverted(c, tokens)
		}
	}
	for _, doc := range di.docs {
		if doc.Abstract != "" {
			di.abstractStats.AddDocument(s.tokenizer.Tokenize(doc.Abstract))
		}
	}
	di.abstract, _ = s.readFile(ctx, filepath.Join(s.prefix, datasetID, ".abstract.md"))
	di.overview, _ = s.readFile(ctx, filepath.Join(s.prefix, datasetID, ".overview.md"))
	return di, nil
}

func (s *FSStore) AddDocument(ctx context.Context, datasetID, name, content string) error {
	if datasetID == "" || name == "" {
		return fmt.Errorf("knowledge: dataset_id and name are required")
	}
	path := s.docPath(datasetID, name)
	if err := s.ws.Write(ctx, path, []byte(content)); err != nil {
		return fmt.Errorf("knowledge: write document: %w", err)
	}
	parsedContent, meta := parseFrontmatter(content)
	doc := &Document{Name: name, Content: parsedContent, Metadata: meta}
	chunks := ChunkDocument(name, parsedContent, s.chunkCfg)
	s.mu.Lock()
	di, ok := s.index[datasetID]
	if !ok {
		di = &datasetIndex{docs: make(map[string]*Document), stats: NewCorpusStats(), abstractStats: NewCorpusStats(), inverted: make(map[string][]posting), vectors: make(map[*Chunk][]float32)}
		s.index[datasetID] = di
	}
	if oldDoc, exists := di.docs[name]; exists {
		s.removeDocChunks(di, oldDoc.Name)
	}
	addedChunks := make([]*Chunk, len(chunks))
	di.docs[name] = doc
	for i := range chunks {
		c := &chunks[i]
		tokens := s.tokenizer.Tokenize(c.Content)
		di.chunks = append(di.chunks, c)
		di.stats.AddDocument(tokens)
		di.addChunkToInverted(c, tokens)
		addedChunks[i] = c
	}
	s.mu.Unlock()
	if s.embedder != nil {
		texts := make([]string, len(addedChunks))
		for i, c := range addedChunks {
			texts[i] = c.Content
		}
		if vecs, err := s.embedder.EmbedBatch(ctx, texts); err == nil && len(vecs) == len(addedChunks) {
			s.mu.Lock()
			if currentDI, ok := s.index[datasetID]; ok && currentDI == di {
				for i, c := range addedChunks {
					currentDI.vectors[c] = vecs[i]
				}
			}
			s.mu.Unlock()
		}
	}
	return nil
}

func (s *FSStore) AddDocuments(ctx context.Context, datasetID string, docs []DocInput) error {
	if datasetID == "" {
		return fmt.Errorf("knowledge: dataset_id is required")
	}
	if len(docs) == 0 {
		return nil
	}
	type prepared struct {
		doc    *Document
		chunks []Chunk
	}
	items := make([]prepared, 0, len(docs))
	for _, d := range docs {
		if d.Name == "" {
			continue
		}
		path := s.docPath(datasetID, d.Name)
		if err := s.ws.Write(ctx, path, []byte(d.Content)); err != nil {
			return fmt.Errorf("knowledge: write document %s: %w", d.Name, err)
		}
		parsedContent, meta := parseFrontmatter(d.Content)
		items = append(items, prepared{
			doc:    &Document{Name: d.Name, Content: parsedContent, Metadata: meta},
			chunks: ChunkDocument(d.Name, parsedContent, s.chunkCfg),
		})
	}
	var allChunks []*Chunk
	s.mu.Lock()
	di, ok := s.index[datasetID]
	if !ok {
		di = &datasetIndex{docs: make(map[string]*Document), stats: NewCorpusStats(), abstractStats: NewCorpusStats(), inverted: make(map[string][]posting), vectors: make(map[*Chunk][]float32)}
		s.index[datasetID] = di
	}
	for _, item := range items {
		if _, exists := di.docs[item.doc.Name]; exists {
			s.removeDocChunks(di, item.doc.Name)
		}
		di.docs[item.doc.Name] = item.doc
		for i := range item.chunks {
			c := &item.chunks[i]
			tokens := s.tokenizer.Tokenize(c.Content)
			di.chunks = append(di.chunks, c)
			di.stats.AddDocument(tokens)
			di.addChunkToInverted(c, tokens)
			allChunks = append(allChunks, c)
		}
	}
	s.mu.Unlock()
	if s.embedder != nil && len(allChunks) > 0 {
		texts := make([]string, len(allChunks))
		for i, c := range allChunks {
			texts[i] = c.Content
		}
		if vecs, err := s.embedder.EmbedBatch(ctx, texts); err == nil && len(vecs) == len(allChunks) {
			s.mu.Lock()
			if currentDI, ok := s.index[datasetID]; ok && currentDI == di {
				for i, c := range allChunks {
					currentDI.vectors[c] = vecs[i]
				}
			}
			s.mu.Unlock()
		}
	}
	return nil
}

// ReindexVectors regenerates vector embeddings for all indexed chunks.
func (s *FSStore) ReindexVectors(ctx context.Context) error {
	if s.embedder == nil {
		return nil
	}
	s.mu.RLock()
	type work struct {
		dsID   string
		chunks []*Chunk
	}
	var tasks []work
	for dsID, di := range s.index {
		chunks := make([]*Chunk, len(di.chunks))
		copy(chunks, di.chunks)
		tasks = append(tasks, work{dsID: dsID, chunks: chunks})
	}
	s.mu.RUnlock()
	for _, t := range tasks {
		if len(t.chunks) == 0 {
			continue
		}
		texts := make([]string, len(t.chunks))
		for i, c := range t.chunks {
			texts[i] = c.Content
		}
		vecs, err := s.embedder.EmbedBatch(ctx, texts)
		if err != nil {
			return fmt.Errorf("knowledge: reindex vectors for %s: %w", t.dsID, err)
		}
		s.mu.Lock()
		if di, ok := s.index[t.dsID]; ok {
			for i, c := range t.chunks {
				if i < len(vecs) {
					di.vectors[c] = vecs[i]
				}
			}
		}
		s.mu.Unlock()
	}
	return nil
}

func (s *FSStore) GetDocument(ctx context.Context, datasetID, name string) (*Document, error) {
	s.mu.RLock()
	if di, ok := s.index[datasetID]; ok {
		if doc, ok := di.docs[name]; ok {
			cp := *doc
			if doc.Metadata != nil {
				cp.Metadata = make(map[string]string, len(doc.Metadata))
				for k, v := range doc.Metadata {
					cp.Metadata[k] = v
				}
			}
			s.mu.RUnlock()
			return &cp, nil
		}
	}
	s.mu.RUnlock()
	path := s.docPath(datasetID, name)
	data, err := s.ws.Read(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("knowledge: get %s/%s: %w", datasetID, name, err)
	}
	content, meta := parseFrontmatter(string(data))
	doc := &Document{Name: name, Content: content, Metadata: meta}
	doc.Abstract, _ = s.readSidecar(ctx, datasetID, name, ".abstract")
	doc.Overview, _ = s.readSidecar(ctx, datasetID, name, ".overview")
	return doc, nil
}

func (s *FSStore) DeleteDocument(ctx context.Context, datasetID, name string) error {
	path := s.docPath(datasetID, name)
	if err := s.ws.Delete(ctx, path); err != nil {
		return fmt.Errorf("knowledge: delete %s/%s: %w", datasetID, name, err)
	}
	_ = s.deleteSidecar(ctx, datasetID, name, ".abstract")
	_ = s.deleteSidecar(ctx, datasetID, name, ".overview")
	s.mu.Lock()
	if di, ok := s.index[datasetID]; ok {
		s.removeDocChunks(di, name)
		delete(di.docs, name)
	}
	s.mu.Unlock()
	return nil
}

func (s *FSStore) ListDocuments(ctx context.Context, datasetID string) ([]Document, error) {
	s.mu.RLock()
	if di, ok := s.index[datasetID]; ok && len(di.docs) > 0 {
		docs := make([]Document, 0, len(di.docs))
		for _, d := range di.docs {
			docs = append(docs, *d)
		}
		s.mu.RUnlock()
		return docs, nil
	}
	s.mu.RUnlock()
	return s.listDocsFromDisk(ctx, datasetID)
}

func (s *FSStore) listDocsFromDisk(ctx context.Context, datasetID string) ([]Document, error) {
	dir := filepath.Join(s.prefix, datasetID)
	entries, err := s.ws.List(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("knowledge: list %s: %w", datasetID, err)
	}
	var docs []Document
	for _, e := range entries {
		if e.IsDir() || !legacyIsMarkdown(e.Name()) {
			continue
		}
		data, err := s.ws.Read(ctx, filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		content, meta := parseFrontmatter(string(data))
		docs = append(docs, Document{Name: e.Name(), Content: content, Metadata: meta})
	}
	return docs, nil
}

// Search performs a two-level search over the in-memory index.
func (s *FSStore) Search(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error) {
	if opts.TopK <= 0 {
		opts.TopK = 5
	}
	if opts.MaxLayer == "" {
		opts.MaxLayer = LayerDetail
	}
	if opts.Threshold <= 0 {
		opts.Threshold = DefaultThreshold
	}
	switch opts.Mode {
	case ModeSemantic:
		return s.searchSemanticOnly(ctx, datasetID, query, opts)
	case ModeHybrid:
		return s.searchHybrid(ctx, datasetID, query, opts)
	default:
		keywords := ExtractKeywords(query, s.tokenizer)
		if len(keywords) == 0 {
			return nil, nil
		}
		if datasetID != "" {
			return s.searchDataset(ctx, datasetID, query, keywords, opts)
		}
		return s.searchAcrossDatasets(ctx, query, keywords, opts)
	}
}

func (s *FSStore) searchSemanticOnly(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error) {
	if s.embedder == nil {
		return s.Search(ctx, datasetID, query, SearchOptions{TopK: opts.TopK, MaxLayer: opts.MaxLayer, Threshold: opts.Threshold})
	}
	qvecs, err := s.embedder.EmbedBatch(ctx, []string{query})
	if err != nil || len(qvecs) == 0 {
		return s.Search(ctx, datasetID, query, SearchOptions{TopK: opts.TopK, MaxLayer: opts.MaxLayer, Threshold: opts.Threshold})
	}
	qvec := qvecs[0]
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []SearchResult
	datasets := make(map[string]*datasetIndex)
	if datasetID != "" {
		if di, ok := s.index[datasetID]; ok {
			datasets[datasetID] = di
		}
	} else {
		for id, di := range s.index {
			datasets[id] = di
		}
	}
	for _, di := range datasets {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		for chunk, vec := range di.vectors {
			sim := cosineSimilarity(qvec, vec)
			if sim >= opts.Threshold {
				results = append(results, SearchResult{
					Content:    chunk.Content,
					Score:      sim,
					DocName:    chunk.DocName,
					ChunkIndex: chunk.Index,
					Layer:      LayerDetail,
				})
			}
		}
	}
	return RankResults(results, opts.TopK), nil
}

func (s *FSStore) searchHybrid(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error) {
	bm25Opts := SearchOptions{TopK: opts.TopK * 2, MaxLayer: opts.MaxLayer, Threshold: opts.Threshold}
	keywords := ExtractKeywords(query, s.tokenizer)
	var bm25Results []SearchResult
	var firstErr error
	if len(keywords) > 0 {
		var err error
		if datasetID != "" {
			bm25Results, err = s.searchDataset(ctx, datasetID, query, keywords, bm25Opts)
		} else {
			bm25Results, err = s.searchAcrossDatasets(ctx, query, keywords, bm25Opts)
		}
		if err != nil {
			firstErr = err
		}
	}
	semanticResults, err := s.searchSemanticOnly(ctx, datasetID, query, SearchOptions{TopK: opts.TopK * 2, MaxLayer: opts.MaxLayer, Threshold: 0})
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if len(bm25Results) == 0 && len(semanticResults) == 0 && firstErr != nil {
		return nil, firstErr
	}
	merged := RRFMerge(bm25Results, semanticResults, 60)
	return RankResults(merged, opts.TopK), nil
}

func (s *FSStore) searchDataset(ctx context.Context, datasetID, _ string, keywords []string, opts SearchOptions) ([]SearchResult, error) {
	s.mu.RLock()
	di, ok := s.index[datasetID]
	if !ok {
		s.mu.RUnlock()
		return nil, nil
	}
	var results []SearchResult
	switch opts.MaxLayer {
	case LayerAbstract:
		for _, doc := range di.docs {
			if ctx.Err() != nil {
				s.mu.RUnlock()
				return nil, ctx.Err()
			}
			if doc.Abstract == "" {
				continue
			}
			score := ScoreText(doc.Abstract, keywords, di.abstractStats, s.tokenizer)
			if score >= opts.Threshold {
				results = append(results, SearchResult{
					Content: doc.Abstract, Score: score, DocName: doc.Name, Layer: LayerAbstract,
				})
			}
		}
	case LayerOverview:
		for _, doc := range di.docs {
			if ctx.Err() != nil {
				s.mu.RUnlock()
				return nil, ctx.Err()
			}
			content := doc.Overview
			if content == "" {
				content = doc.Abstract
			}
			if content == "" {
				continue
			}
			score := ScoreText(content, keywords, di.stats, s.tokenizer)
			layer := LayerOverview
			if doc.Overview == "" {
				layer = LayerAbstract
			}
			if score >= opts.Threshold {
				results = append(results, SearchResult{
					Content: content, Score: score, DocName: doc.Name, Layer: layer,
				})
			}
		}
	default:
		results = di.searchL2(keywords, opts.Threshold)
	}
	s.mu.RUnlock()
	return RankResults(results, opts.TopK), nil
}

func (s *FSStore) searchAcrossDatasets(ctx context.Context, _ string, keywords []string, opts SearchOptions) ([]SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var dsScores []dsScore
	for dsID, di := range s.index {
		if di.abstract != "" {
			score := ScoreText(di.abstract, keywords, di.abstractStats, s.tokenizer)
			dsScores = append(dsScores, dsScore{dsID, score})
		} else {
			dsScores = append(dsScores, dsScore{dsID, 0.1})
		}
	}
	sortDSScores(dsScores)
	maxDS := 3
	if len(dsScores) < maxDS {
		maxDS = len(dsScores)
	}
	var results []SearchResult
	for _, ds := range dsScores[:maxDS] {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		di := s.index[ds.id]
		switch opts.MaxLayer {
		case LayerAbstract:
			for _, doc := range di.docs {
				if doc.Abstract != "" {
					score := ScoreText(doc.Abstract, keywords, di.abstractStats, s.tokenizer)
					if score >= opts.Threshold {
						results = append(results, SearchResult{
							Content: doc.Abstract, Score: score, DocName: doc.Name, Layer: LayerAbstract,
						})
					}
				}
			}
		case LayerOverview:
			for _, doc := range di.docs {
				content := doc.Overview
				if content == "" {
					content = doc.Abstract
				}
				if content == "" {
					continue
				}
				score := ScoreText(content, keywords, di.stats, s.tokenizer)
				if score >= opts.Threshold {
					results = append(results, SearchResult{
						Content: content, Score: score, DocName: doc.Name, Layer: LayerOverview,
					})
				}
			}
		default:
			results = append(results, di.searchL2(keywords, opts.Threshold)...)
		}
	}
	return RankResults(results, opts.TopK), nil
}

type dsScore struct {
	id    string
	score float64
}

func sortDSScores(scores []dsScore) {
	for i := 1; i < len(scores); i++ {
		for j := i; j > 0 && scores[j].score > scores[j-1].score; j-- {
			scores[j], scores[j-1] = scores[j-1], scores[j]
		}
	}
}

func (s *FSStore) Abstract(ctx context.Context, datasetID, name string) (string, error) {
	s.mu.RLock()
	if di, ok := s.index[datasetID]; ok {
		if doc, ok := di.docs[name]; ok {
			s.mu.RUnlock()
			return doc.Abstract, nil
		}
	}
	s.mu.RUnlock()
	return s.readSidecar(ctx, datasetID, name, ".abstract")
}

func (s *FSStore) Overview(ctx context.Context, datasetID, name string) (string, error) {
	s.mu.RLock()
	if di, ok := s.index[datasetID]; ok {
		if doc, ok := di.docs[name]; ok {
			s.mu.RUnlock()
			return doc.Overview, nil
		}
	}
	s.mu.RUnlock()
	return s.readSidecar(ctx, datasetID, name, ".overview")
}

func (s *FSStore) DatasetAbstract(_ context.Context, datasetID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if di, ok := s.index[datasetID]; ok {
		return di.abstract, nil
	}
	return "", nil
}

func (s *FSStore) DatasetOverview(_ context.Context, datasetID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if di, ok := s.index[datasetID]; ok {
		return di.overview, nil
	}
	return "", nil
}

func (s *FSStore) SetDocAbstract(datasetID, name, abstract string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if di, ok := s.index[datasetID]; ok {
		if doc, ok := di.docs[name]; ok {
			if doc.Abstract != "" {
				di.abstractStats.RemoveDocument(s.tokenizer.Tokenize(doc.Abstract))
			}
			doc.Abstract = abstract
			if abstract != "" {
				di.abstractStats.AddDocument(s.tokenizer.Tokenize(abstract))
			}
		}
	}
}

func (s *FSStore) SetDocOverview(datasetID, name, overview string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if di, ok := s.index[datasetID]; ok {
		if doc, ok := di.docs[name]; ok {
			doc.Overview = overview
		}
	}
}

func (s *FSStore) SetDatasetAbstract(datasetID, abstract string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if di, ok := s.index[datasetID]; ok {
		di.abstract = abstract
	}
}

func (s *FSStore) SetDatasetOverview(datasetID, overview string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if di, ok := s.index[datasetID]; ok {
		di.overview = overview
	}
}

func (s *FSStore) removeDocChunks(di *datasetIndex, docName string) {
	di.removeFromInverted(docName)
	var kept []*Chunk
	for _, c := range di.chunks {
		if c.DocName == docName {
			di.stats.RemoveDocument(s.tokenizer.Tokenize(c.Content))
			delete(di.vectors, c)
		} else {
			kept = append(kept, c)
		}
	}
	di.chunks = kept
}

func (s *FSStore) docPath(datasetID, name string) string {
	if !legacyIsMarkdown(name) {
		name += ".md"
	}
	return filepath.Join(s.prefix, datasetID, name)
}

func (s *FSStore) readSidecar(ctx context.Context, datasetID, name, ext string) (string, error) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	path := filepath.Join(s.prefix, datasetID, base+ext)
	return s.readFile(ctx, path)
}

func (s *FSStore) deleteSidecar(ctx context.Context, datasetID, name, ext string) error {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	path := filepath.Join(s.prefix, datasetID, base+ext)
	return s.ws.Delete(ctx, path)
}

func (s *FSStore) readFile(ctx context.Context, path string) (string, error) {
	exists, err := s.ws.Exists(ctx, path)
	if err != nil || !exists {
		return "", err
	}
	data, err := s.ws.Read(ctx, path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteSidecar writes a per-document sidecar file (e.g. ".abstract",
// ".overview") used to persist layered context derived externally.
func (s *FSStore) WriteSidecar(ctx context.Context, datasetID, name, ext, content string) error {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	path := filepath.Join(s.prefix, datasetID, base+ext)
	return s.ws.Write(ctx, path, []byte(content))
}

// WriteDatasetFile writes a dataset-level file (e.g. .abstract.md, .overview.md).
func (s *FSStore) WriteDatasetFile(ctx context.Context, datasetID, filename, content string) error {
	path := filepath.Join(s.prefix, datasetID, filename)
	return s.ws.Write(ctx, path, []byte(content))
}

// WorkspaceRoot exposes the underlying workspace root when available.
//
// Returns "" if the workspace does not implement Root().
func (s *FSStore) WorkspaceRoot() string {
	if rw, ok := s.ws.(interface{ Root() string }); ok {
		return rw.Root()
	}
	return ""
}

// Prefix returns the FSStore directory prefix beneath WorkspaceRoot.
func (s *FSStore) Prefix() string { return s.prefix }

// =============================================================================
// RetrievalStore (retrieval.Index-backed Store implementation)
// =============================================================================

// RetrievalStore is a Store implementation backed by a retrieval.Index.
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
//
// Deprecated: see RetrievalStore.
type RetrievalStoreOption func(*RetrievalStore)

// WithRetrievalEmbedder sets the embedder used to vectorize chunks at write time.
//
// Deprecated: pass embedder through factory.WithRetrievalEmbedder.
func WithRetrievalEmbedder(e embedding.Embedder) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.embedder = e }
}

// WithRetrievalPipeline overrides the default pipeline.Knowledge(emb, nil).
//
// Deprecated: factory.NewRetrieval owns the pipeline now.
func WithRetrievalPipeline(p *pipeline.Pipeline) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.pipeline = p }
}

// WithRetrievalChunkConfig overrides the default chunk config.
//
// Deprecated: configure ChunkConfig via factory.WithRetrievalChunker.
func WithRetrievalChunkConfig(c ChunkConfig) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.chunkCfg = c }
}

// WithRetrievalTokenizer overrides the BM25 tokenizer.
//
// Deprecated: factory wires the tokenizer through textsearch.
func WithRetrievalTokenizer(t Tokenizer) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.tokenizer = t }
}

// NewRetrievalStore wires a Store to a retrieval.Index.
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

// Index exposes the underlying retrieval.Index.
func (s *RetrievalStore) Index() retrieval.Index { return s.idx }

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

func (s *RetrievalStore) AddDocument(ctx context.Context, datasetID, name, content string) error {
	body, meta := parseFrontmatter(content)
	if err := s.upsertDocChunks(ctx, datasetID, name, body, meta); err != nil {
		return err
	}
	return s.upsertDocMeta(ctx, datasetID, name, meta)
}

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

func (s *RetrievalStore) SetAbstract(ctx context.Context, datasetID, name, abstract string) error {
	return s.upsertLayer(ctx, datasetID, name, LayerAbstract, abstract)
}

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

func (s *RetrievalStore) SetDatasetAbstract(ctx context.Context, datasetID, abstract string) error {
	return s.idx.Upsert(ctx, datasetMetaNS, []retrieval.Doc{{
		ID: "abstract:" + saneNS(datasetID), Content: abstract,
		Metadata:  map[string]any{"dataset": datasetID, "layer": string(LayerAbstract)},
		Timestamp: s.now().UTC(),
	}})
}

func (s *RetrievalStore) SetDatasetOverview(ctx context.Context, datasetID, overview string) error {
	return s.idx.Upsert(ctx, datasetMetaNS, []retrieval.Doc{{
		ID: "overview:" + saneNS(datasetID), Content: overview,
		Metadata:  map[string]any{"dataset": datasetID, "layer": string(LayerOverview)},
		Timestamp: s.now().UTC(),
	}})
}

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

func (s *RetrievalStore) Search(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error) {
	topK := opts.TopK
	if topK <= 0 {
		topK = 10
	}
	threshold := opts.Threshold
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

func (s *RetrievalStore) Abstract(ctx context.Context, datasetID, name string) (string, error) {
	d, ok, err := s.getByID(ctx, docMetaNS(datasetID), string(LayerAbstract)+":"+name)
	if err != nil || !ok {
		return "", err
	}
	return d.Content, nil
}

func (s *RetrievalStore) Overview(ctx context.Context, datasetID, name string) (string, error) {
	d, ok, err := s.getByID(ctx, docMetaNS(datasetID), string(LayerOverview)+":"+name)
	if err != nil || !ok {
		return "", err
	}
	return d.Content, nil
}

func (s *RetrievalStore) DatasetAbstract(ctx context.Context, datasetID string) (string, error) {
	d, ok, err := s.getByID(ctx, datasetMetaNS, "abstract:"+saneNS(datasetID))
	if err != nil || !ok {
		return "", err
	}
	return d.Content, nil
}

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

// =============================================================================
// CachedStore (Store TTL+LRU wrapper)
// =============================================================================

// CacheOption configures a CachedStore.
//
// Deprecated: see CachedStore.
type CacheOption func(*CachedStore)

// WithTTL sets the cache time-to-live.
//
// Deprecated: see CachedStore.
func WithTTL(d time.Duration) CacheOption {
	return func(s *CachedStore) { s.ttl = d }
}

// WithMaxItems sets the maximum number of cached items.
//
// Deprecated: see CachedStore.
func WithMaxItems(n int) CacheOption {
	return func(s *CachedStore) { s.maxItems = n }
}

type cacheEntry struct {
	key     string
	value   any
	expiry  time.Time
	element *list.Element
}

// CachedStore wraps a Store with TTL + LRU caching for read operations.
//
// Deprecated: caching now lives inside Service / repository implementations
// where appropriate; the indirection no longer earns its keep at the
// orchestration layer. Removed in v0.3.0.
type CachedStore struct {
	inner    Store
	mu       sync.RWMutex
	items    map[string]*cacheEntry
	order    *list.List
	ttl      time.Duration
	maxItems int
}

// NewCachedStore wraps inner with caching.
//
// Deprecated: see CachedStore. Removed in v0.3.0.
func NewCachedStore(inner Store, opts ...CacheOption) *CachedStore {
	s := &CachedStore{
		inner:    inner,
		items:    make(map[string]*cacheEntry),
		order:    list.New(),
		ttl:      5 * time.Minute,
		maxItems: 1000,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *CachedStore) get(key string) (any, bool) {
	s.mu.RLock()
	entry, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiry) {
		s.mu.Lock()
		s.removeLocked(key)
		s.mu.Unlock()
		return nil, false
	}
	s.mu.Lock()
	s.order.MoveToFront(entry.element)
	s.mu.Unlock()
	return entry.value, true
}

func (s *CachedStore) set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.items[key]; ok {
		entry.value = value
		entry.expiry = time.Now().Add(s.ttl)
		s.order.MoveToFront(entry.element)
		return
	}
	for len(s.items) >= s.maxItems {
		back := s.order.Back()
		if back == nil {
			break
		}
		s.removeLocked(back.Value.(string))
	}
	el := s.order.PushFront(key)
	s.items[key] = &cacheEntry{
		key: key, value: value, expiry: time.Now().Add(s.ttl), element: el,
	}
}

func (s *CachedStore) removeLocked(key string) {
	entry, ok := s.items[key]
	if !ok {
		return
	}
	s.order.Remove(entry.element)
	delete(s.items, key)
}

func (s *CachedStore) evictDataset(datasetID string) {
	prefix := "doc:" + datasetID + "/"
	searchPrefix := "search:" + datasetID + "|"
	absPrefix := "abs:" + datasetID + "/"
	ovPrefix := "ov:" + datasetID + "/"
	dsAbs := "dsabs:" + datasetID
	dsOv := "dsov:" + datasetID
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.items {
		if hasAnyPrefix(key, prefix, searchPrefix, absPrefix, ovPrefix) || key == dsAbs || key == dsOv {
			s.removeLocked(key)
		}
	}
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}

func (s *CachedStore) AddDocument(ctx context.Context, datasetID, name, content string) error {
	if err := s.inner.AddDocument(ctx, datasetID, name, content); err != nil {
		return err
	}
	s.evictDataset(datasetID)
	return nil
}

func (s *CachedStore) AddDocuments(ctx context.Context, datasetID string, docs []DocInput) error {
	if err := s.inner.AddDocuments(ctx, datasetID, docs); err != nil {
		return err
	}
	s.evictDataset(datasetID)
	return nil
}

func (s *CachedStore) GetDocument(ctx context.Context, datasetID, name string) (*Document, error) {
	key := fmt.Sprintf("doc:%s/%s", datasetID, name)
	if v, ok := s.get(key); ok {
		doc, ok := v.(Document)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.GetDocument(ctx, datasetID, name)
		}
		return &doc, nil
	}
	doc, err := s.inner.GetDocument(ctx, datasetID, name)
	if err != nil {
		return nil, err
	}
	s.set(key, *doc)
	return doc, nil
}

func (s *CachedStore) DeleteDocument(ctx context.Context, datasetID, name string) error {
	if err := s.inner.DeleteDocument(ctx, datasetID, name); err != nil {
		return err
	}
	s.evictDataset(datasetID)
	return nil
}

func (s *CachedStore) ListDocuments(ctx context.Context, datasetID string) ([]Document, error) {
	return s.inner.ListDocuments(ctx, datasetID)
}

func (s *CachedStore) Search(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error) {
	key := fmt.Sprintf("search:%s|%s|%d|%s|%f|%s", datasetID, query, opts.TopK, opts.MaxLayer, opts.Threshold, opts.Mode)
	if v, ok := s.get(key); ok {
		results, ok := v.([]SearchResult)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.Search(ctx, datasetID, query, opts)
		}
		cp := make([]SearchResult, len(results))
		copy(cp, results)
		return cp, nil
	}
	results, err := s.inner.Search(ctx, datasetID, query, opts)
	if err != nil {
		return nil, err
	}
	s.set(key, results)
	return results, nil
}

func (s *CachedStore) Abstract(ctx context.Context, datasetID, name string) (string, error) {
	key := fmt.Sprintf("abs:%s/%s", datasetID, name)
	if v, ok := s.get(key); ok {
		val, ok := v.(string)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.Abstract(ctx, datasetID, name)
		}
		return val, nil
	}
	val, err := s.inner.Abstract(ctx, datasetID, name)
	if err != nil {
		return "", err
	}
	s.set(key, val)
	return val, nil
}

func (s *CachedStore) Overview(ctx context.Context, datasetID, name string) (string, error) {
	key := fmt.Sprintf("ov:%s/%s", datasetID, name)
	if v, ok := s.get(key); ok {
		val, ok := v.(string)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.Overview(ctx, datasetID, name)
		}
		return val, nil
	}
	val, err := s.inner.Overview(ctx, datasetID, name)
	if err != nil {
		return "", err
	}
	s.set(key, val)
	return val, nil
}

func (s *CachedStore) DatasetAbstract(ctx context.Context, datasetID string) (string, error) {
	key := "dsabs:" + datasetID
	if v, ok := s.get(key); ok {
		val, ok := v.(string)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.DatasetAbstract(ctx, datasetID)
		}
		return val, nil
	}
	val, err := s.inner.DatasetAbstract(ctx, datasetID)
	if err != nil {
		return "", err
	}
	s.set(key, val)
	return val, nil
}

func (s *CachedStore) DatasetOverview(ctx context.Context, datasetID string) (string, error) {
	key := "dsov:" + datasetID
	if v, ok := s.get(key); ok {
		val, ok := v.(string)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.DatasetOverview(ctx, datasetID)
		}
		return val, nil
	}
	val, err := s.inner.DatasetOverview(ctx, datasetID)
	if err != nil {
		return "", err
	}
	s.set(key, val)
	return val, nil
}

// EvictDataset removes all cached entries for a dataset.
func (s *CachedStore) EvictDataset(datasetID string) {
	s.evictDataset(datasetID)
}

// =============================================================================
// Legacy Reloader / ChangeNotifier
// =============================================================================

// ChangeNotifier emits an opaque event whenever the underlying source changes.
//
// Deprecated: use EventNotifier (typed ChangeEvent stream) with
// EventReloader. Removed in v0.3.0.
type ChangeNotifier interface {
	Events() <-chan struct{}
	Close() error
}

// Reloader debounces ChangeNotifier events and triggers Rebuild on a
// stable trailing edge.
//
// Deprecated: use EventReloader (typed events + scope-aware Service.Rebuild
// + serialised execution). Removed in v0.3.0.
type Reloader struct {
	notifier ChangeNotifier
	rebuild  func(ctx context.Context) error
	debounce time.Duration

	mu      sync.Mutex
	pending bool
	wg      sync.WaitGroup
	stop    chan struct{}
}

// NewReloader wires a ChangeNotifier to a rebuild callback.
//
// Deprecated: use NewEventReloader(target Rebuilder, notifier EventNotifier, opts).
// Removed in v0.3.0.
func NewReloader(store *FSStore, notifier ChangeNotifier, opts ReloaderOptions) *Reloader {
	d := opts.Debounce
	if d <= 0 {
		d = 500 * time.Millisecond
	}
	rebuild := opts.Rebuild
	if rebuild == nil && store != nil {
		rebuild = store.BuildIndex
	}
	return &Reloader{
		notifier: notifier,
		rebuild:  rebuild,
		debounce: d,
		stop:     make(chan struct{}),
	}
}

// Run blocks until Close is called or ctx is cancelled.
func (r *Reloader) Run(ctx context.Context) error {
	if r.notifier == nil || r.rebuild == nil {
		return nil
	}
	var timer *time.Timer
	r.wg.Add(1)
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-r.stop:
			if timer != nil {
				timer.Stop()
			}
			return nil
		case _, ok := <-r.notifier.Events():
			if !ok {
				return nil
			}
			r.mu.Lock()
			if !r.pending {
				r.pending = true
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(r.debounce, func() {
					r.mu.Lock()
					r.pending = false
					r.mu.Unlock()
					rebuildCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()
					_ = r.rebuild(rebuildCtx)
				})
			}
			r.mu.Unlock()
		}
	}
}

// Close stops Run and the underlying ChangeNotifier.
func (r *Reloader) Close() error {
	close(r.stop)
	r.wg.Wait()
	if r.notifier != nil {
		return r.notifier.Close()
	}
	return nil
}

// =============================================================================
// Legacy graph node (KnowledgeNode + KnowledgeConfig + KnowledgeNodeSchema)
// =============================================================================
//
// Removed in this branch. All "knowledge" graph node implementations now
// live in github.com/GizClaw/flowcraft/sdk/graph/node/knowledgenode:
//
//	knowledge.KnowledgeServiceNode    -> knowledgenode.Node
//	knowledge.KnowledgeNodeConfig     -> knowledgenode.Config
//	knowledge.KnowledgeNodeConfigFromMap -> knowledgenode.ConfigFromMap
//	knowledge.RegisterServiceNode     -> knowledgenode.Register(*node.Factory, *Service)
//	knowledge.KnowledgeServiceNodeSchema -> (deleted; UI metadata is no longer SDK-owned)
//	knowledge.NewKnowledgeNode        -> knowledgenode.New(svc, knowledgenode.Config{...})
//	knowledge.KnowledgeConfigFromMap  -> knowledgenode.ConfigFromMap
//	knowledge.RegisterNode            -> knowledgenode.Register
//	knowledge.KnowledgeNodeSchema     -> (deleted)

// =============================================================================
// Legacy LLM tools
// =============================================================================

// NewSearchTool returns the legacy "knowledge_search" LLM tool.
//
// Deprecated: use NewSearchServiceTool(*Service). Removed in v0.3.0.
func NewSearchTool(ks Store) tool.Tool {
	return tool.FuncTool(
		tool.DefineSchema("knowledge_search",
			"Search the knowledge base using keyword matching. "+
				"Use specific, concrete keywords (e.g. node type names, error messages) "+
				"rather than abstract queries for best results. "+
				"Automatically searches across all datasets and returns ranked results.",
			tool.Property("query", "string", "Search query"),
			tool.Property("top_k", "integer", "Number of results to return (default 5)"),
		).Required("query").Build(),
		func(ctx context.Context, args string) (string, error) {
			if ks == nil {
				return "", errdefs.NotAvailablef("knowledge store not available")
			}
			var p struct {
				Query string `json:"query"`
				TopK  int    `json:"top_k"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if p.TopK <= 0 {
				p.TopK = 5
			}
			results, err := ks.Search(ctx, "", p.Query, SearchOptions{TopK: p.TopK})
			if err != nil {
				return "", err
			}
			data, err := json.Marshal(results)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
	)
}

// NewAddTool returns the legacy "knowledge_add" LLM tool.
//
// Deprecated: use NewPutServiceTool(*Service). Removed in v0.3.0.
func NewAddTool(ks Store) tool.Tool {
	return tool.FuncTool(
		tool.DefineSchema("knowledge_add",
			"Add a document to the knowledge base. "+
				"Use this to persist reusable knowledge such as troubleshooting conclusions, "+
				"best practices, or design decisions that may benefit future conversations. "+
				"Do NOT use this for personal preferences or temporary notes.",
			tool.Property("dataset_id", "string", "Target dataset ID (default: \"default\")"),
			tool.Property("name", "string", "Document name (include .md suffix)"),
			tool.Property("content", "string", "Document content in markdown"),
		).Required("name", "content").Build(),
		func(ctx context.Context, args string) (string, error) {
			if ks == nil {
				return "", errdefs.NotAvailablef("knowledge store not available")
			}
			var p struct {
				DatasetID string `json:"dataset_id"`
				Name      string `json:"name"`
				Content   string `json:"content"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if p.DatasetID == "" {
				p.DatasetID = defaultDatasetID
			}
			if err := ks.AddDocument(ctx, p.DatasetID, p.Name, p.Content); err != nil {
				return "", err
			}
			resp, _ := json.Marshal(map[string]string{
				"status":     "ok",
				"dataset_id": p.DatasetID,
				"name":       p.Name,
			})
			return string(resp), nil
		},
	)
}

// Compile-time assertions that the legacy types still implement Store.
var (
	_ Store = (*FSStore)(nil)
	_ Store = (*RetrievalStore)(nil)
	_ Store = (*CachedStore)(nil)
)
