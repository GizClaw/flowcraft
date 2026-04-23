package knowledge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

type posting struct {
	chunk *Chunk
	tf    int
	dl    int
}

// datasetIndex holds the in-memory index for a single dataset.
type datasetIndex struct {
	chunks        []*Chunk
	docs          map[string]*Document
	stats         *CorpusStats
	abstractStats *CorpusStats
	abstract      string // dataset-level L0
	overview      string // dataset-level L1
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
type FSStoreOption func(*FSStore)

// WithTokenizer sets the tokenizer for search and indexing.
func WithTokenizer(t Tokenizer) FSStoreOption {
	return func(s *FSStore) { s.tokenizer = t }
}

// WithChunkConfig sets the chunking configuration.
func WithChunkConfig(cfg ChunkConfig) FSStoreOption {
	return func(s *FSStore) { s.chunkCfg = cfg }
}

// WithEmbedder sets the embedder for semantic/hybrid search.
func WithEmbedder(e Embedder) FSStoreOption {
	return func(s *FSStore) { s.embedder = e }
}

// FSStore implements Store using a Workspace-backed file tree.
// Thread-safe via sync.RWMutex.
//
// FSStore is intentionally side-effect free with respect to layered
// context: AddDocument persists raw content + builds the BM25/vector
// index, but does not synthesize L0/L1. Callers are expected to drive
// summarization explicitly via the GenerateDocumentContext /
// GenerateDatasetContext helpers and then publish results back through
// SetDocAbstract / SetDocOverview / SetDatasetAbstract / SetDatasetOverview
// (and WriteSidecar / WriteDatasetFile for persistence).
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

		// Load L0/L1 if cached
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

	// Load dataset-level L0/L1
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
// Safe to call after BuildIndex to restore semantic/hybrid search capability.
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
	// Clean up sidecars
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
		if e.IsDir() || !isMarkdown(e.Name()) {
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

// Search performs a two-level search: if datasetID is empty, first filter
// datasets by L0, then search within top datasets; otherwise search directly.
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

	// Semantic side uses threshold 0 intentionally: let RRF decide final ranking
	// rather than pre-filtering by absolute cosine similarity.
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

	// First level: rank datasets by their L0 abstract
	var dsScores []dsScore
	for dsID, di := range s.index {
		if di.abstract != "" {
			score := ScoreText(di.abstract, keywords, di.abstractStats, s.tokenizer)
			dsScores = append(dsScores, dsScore{dsID, score})
		} else {
			dsScores = append(dsScores, dsScore{dsID, 0.1}) // include unscored datasets
		}
	}
	// Sort datasets by score, take top 3
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

// --- Layered reads ---

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

// SetDocAbstract updates the in-memory abstract for a document and refreshes
// abstract-layer corpus statistics. Intended to be called after deriving a
// new L0 (e.g. via GenerateDocumentContext); pair with WriteSidecar to make
// the change durable.
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

// SetDocOverview updates the in-memory overview for a document.
func (s *FSStore) SetDocOverview(datasetID, name, overview string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if di, ok := s.index[datasetID]; ok {
		if doc, ok := di.docs[name]; ok {
			doc.Overview = overview
		}
	}
}

// SetDatasetAbstract updates the in-memory dataset-level abstract.
func (s *FSStore) SetDatasetAbstract(datasetID, abstract string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if di, ok := s.index[datasetID]; ok {
		di.abstract = abstract
	}
}

// SetDatasetOverview updates the in-memory dataset-level overview.
func (s *FSStore) SetDatasetOverview(datasetID, overview string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if di, ok := s.index[datasetID]; ok {
		di.overview = overview
	}
}

// --- helpers ---

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
	if !isMarkdown(name) {
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

func isMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown" || ext == ".txt"
}
