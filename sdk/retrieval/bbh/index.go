package bbh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blevesearch/bleve/v2"
	_ "github.com/blevesearch/bleve/v2/analysis/analyzer/simple"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/en"
	"github.com/coder/hnsw"
	"github.com/dgraph-io/badger/v4"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/scoring"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

const (
	defaultMinSearchWindow = 128
	defaultMaxListPageSize = 10_000
	badgerDocPrefix        = "doc/"
	badgerNamespacePrefix  = "ns/"
	bleveContentField      = "content"
	badgerDir              = "badger"
	bleveDir               = "bleve"
	hnswDir                = "hnsw"
	hnswGraphExt           = ".graph"
)

// Index implements retrieval.Index using a shared Badger doc store plus
// per-namespace Bleve and HNSW indexes.
type Index struct {
	db      *badger.DB
	root    string
	cfg     Config
	closed  atomic.Bool
	shardMu sync.Mutex
	shards  map[string]*shard
}

var _ retrieval.Index = (*Index)(nil)

type shard struct {
	namespace string
	bleveDir  string
	hnswPath  string
	text      bleve.Index
	graph     *hnsw.SavedGraph[string]
	mu        sync.RWMutex

	graphDirty bool
	flushStop  chan struct{}
	flushDone  chan error
	closeOnce  sync.Once
}

type textDoc struct {
	Content string `json:"content"`
}

// New constructs an Index rooted directly in a local workspace. Badger, Bleve,
// and HNSW are path-backed stores, so callers must provide an already-prefixed
// LocalWorkspace rather than a generic workspace implementation.
func New(ws *sdkworkspace.LocalWorkspace, opts ...Option) (*Index, error) {
	if ws == nil {
		return nil, errdefs.Validationf("retrieval/bbh: workspace is nil")
	}
	root := ws.Root()
	cfg, err := resolveConfig(root, opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, bleveDir), 0o755); err != nil {
		return nil, fmt.Errorf("retrieval/bbh: create bleve root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, hnswDir), 0o755); err != nil {
		return nil, fmt.Errorf("retrieval/bbh: create hnsw root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, badgerDir), 0o755); err != nil {
		return nil, fmt.Errorf("retrieval/bbh: create badger root: %w", err)
	}
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(root, badgerDir)).WithLogger(nil))
	if err != nil {
		return nil, fmt.Errorf("retrieval/bbh: open badger: %w", err)
	}
	return &Index{
		db:     db,
		root:   root,
		cfg:    cfg,
		shards: make(map[string]*shard),
	}, nil
}

// Close releases all open namespace indexes and the shared Badger store.
func (idx *Index) Close() error {
	if !idx.closed.CompareAndSwap(false, true) {
		return nil
	}
	idx.shardMu.Lock()
	shards := make([]*shard, 0, len(idx.shards))
	for _, sh := range idx.shards {
		shards = append(shards, sh)
	}
	idx.shards = map[string]*shard{}
	idx.shardMu.Unlock()

	var errs []error
	for _, sh := range shards {
		errs = append(errs, sh.close())
	}
	errs = append(errs, idx.db.Close())
	return errors.Join(errs...)
}

// Capabilities implements retrieval.Index.
func (idx *Index) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{
		BM25:   true,
		Vector: true,
		Hybrid: true,

		FilterPushdown: true,
		MaxFilterDepth: -1,
		SupportedOps: []string{
			"eq", "neq", "in", "nin", "range", "exists", "missing",
			"contains", "icontains", "contains_any", "contains_all",
			"and", "or", "not", "match",
		},

		BatchUpsertMax: 0,
		WriteIsAtomic:  false,

		MaxListPageSize:      defaultMaxListPageSize,
		NativeDeleteByFilter: true,
		SupportedListOrders: []retrieval.ListOrderBy{
			retrieval.OrderByTimestampDesc,
			retrieval.OrderByTimestampAsc,
			retrieval.OrderByIDAsc,
		},

		ReadAfterWrite: true,
		Distributed:    false,
		Extensions: retrieval.ExtensionCapabilities{
			DocGetter:      true,
			Filterable:     true,
			Iterable:       true,
			Count:          true,
			DeleteByFilter: true,
			DropNamespace:  true,
		},
	}
}

// SupportsFilter implements retrieval.Filterable. BBH evaluates the full
// filter surface against Badger-loaded docs after candidate generation.
func (idx *Index) SupportsFilter(retrieval.Filter) bool { return true }

func (idx *Index) ensureOpen() error {
	if idx.closed.Load() {
		return errdefs.NotAvailablef("retrieval/bbh: index is closed")
	}
	return nil
}

func (idx *Index) ensureShard(namespace string) (*shard, error) {
	if err := idx.ensureOpen(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(namespace) == "" {
		return nil, errdefs.Validationf("retrieval/bbh: namespace is required")
	}
	idx.shardMu.Lock()
	defer idx.shardMu.Unlock()
	if sh, ok := idx.shards[namespace]; ok {
		return sh, nil
	}
	token := safeToken(namespace)
	blevePath := filepath.Join(idx.root, bleveDir, token)
	hnswPath := filepath.Join(idx.root, hnswDir, token+hnswGraphExt)
	text, err := openBleve(blevePath, idx.cfg.Bleve)
	if err != nil {
		return nil, err
	}
	graph, err := hnsw.LoadSavedGraph[string](hnswPath)
	if err != nil {
		_ = text.Close()
		return nil, fmt.Errorf("retrieval/bbh: open hnsw namespace %q: %w", namespace, err)
	}
	sh := &shard{
		namespace: namespace,
		bleveDir:  blevePath,
		hnswPath:  hnswPath,
		text:      text,
		graph:     graph,
		flushStop: make(chan struct{}),
		flushDone: make(chan error, 1),
	}
	go sh.flushLoop(idx.cfg.HNSW.FlushInterval.Duration)
	idx.shards[namespace] = sh
	return sh, nil
}

func (sh *shard) flushLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ticker.C:
			if err := sh.flushGraph(); err != nil {
				lastErr = err
			} else {
				lastErr = nil
			}
		case <-sh.flushStop:
			if err := sh.flushGraph(); err == nil {
				lastErr = nil
			} else {
				lastErr = err
			}
			sh.flushDone <- lastErr
			close(sh.flushDone)
			return
		}
	}
}

func (sh *shard) flushGraph() error {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if !sh.graphDirty || sh.graph == nil {
		return nil
	}
	if err := sh.graph.Save(); err != nil {
		return err
	}
	sh.graphDirty = false
	return nil
}

func (sh *shard) close() error {
	var err error
	sh.closeOnce.Do(func() {
		close(sh.flushStop)
		err = <-sh.flushDone
		sh.mu.Lock()
		if sh.text != nil {
			err = errors.Join(err, sh.text.Close())
		}
		sh.mu.Unlock()
	})
	return err
}

func openBleve(path string, cfg BleveConfig) (bleve.Index, error) {
	if _, err := os.Stat(path); err == nil {
		return bleve.Open(path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	mapping := bleve.NewIndexMapping()
	if cfg.Analyzer == bleveAnalyzerGojieba {
		if err := configureGojiebaAnalyzer(mapping, cfg.Gojieba); err != nil {
			return nil, err
		}
	}
	mapping.DefaultAnalyzer = cfg.Analyzer
	mapping.DefaultMapping = bleve.NewDocumentStaticMapping()
	fm := bleve.NewTextFieldMapping()
	fm.Analyzer = cfg.Analyzer
	fm.Store = false
	fm.Index = true
	fm.IncludeInAll = true
	mapping.DefaultMapping.AddFieldMappingsAt(bleveContentField, fm)
	return bleve.New(path, mapping)
}

func safeToken(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func docKey(namespace, id string) []byte {
	return []byte(badgerDocPrefix + safeToken(namespace) + "/" + safeToken(id))
}

func docPrefix(namespace string) []byte {
	return []byte(badgerDocPrefix + safeToken(namespace) + "/")
}

func nsPrefix(namespace string) []byte {
	return []byte(badgerNamespacePrefix + safeToken(namespace) + "/")
}

func (idx *Index) putDoc(namespace string, d retrieval.Doc) error {
	if d.Timestamp.IsZero() {
		d.Timestamp = time.Now().UTC()
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("retrieval/bbh: marshal doc %s: %w", d.ID, err)
	}
	return idx.db.Update(func(txn *badger.Txn) error {
		return txn.Set(docKey(namespace, d.ID), raw)
	})
}

func (idx *Index) getDoc(namespace, id string) (retrieval.Doc, bool, error) {
	var out retrieval.Doc
	err := idx.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(docKey(namespace, id))
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return nil
			}
			return err
		}
		return item.Value(func(v []byte) error {
			return json.Unmarshal(v, &out)
		})
	})
	if err != nil {
		return retrieval.Doc{}, false, err
	}
	if out.ID == "" {
		return retrieval.Doc{}, false, nil
	}
	return out, true, nil
}

// Get implements retrieval.DocGetter.
func (idx *Index) Get(ctx context.Context, namespace, id string) (retrieval.Doc, bool, error) {
	if err := ctx.Err(); err != nil {
		return retrieval.Doc{}, false, err
	}
	if err := idx.ensureOpen(); err != nil {
		return retrieval.Doc{}, false, err
	}
	return idx.getDoc(namespace, id)
}

// Upsert implements retrieval.Index.
func (idx *Index) Upsert(ctx context.Context, namespace string, docs []retrieval.Doc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(docs) == 0 {
		return nil
	}
	docs = dedupeDocs(docs)
	for _, d := range docs {
		if strings.TrimSpace(d.ID) == "" {
			return errdefs.Validationf("retrieval/bbh: doc id is required")
		}
	}
	sh, err := idx.ensureShard(namespace)
	if err != nil {
		return err
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if err := validateVectorBatchLocked(sh, docs); err != nil {
		return err
	}
	batch := sh.text.NewBatch()
	graphDirty := false
	rebuildGraph := false
	for _, d := range docs {
		if d.Timestamp.IsZero() {
			d.Timestamp = time.Now().UTC()
		}
		if err := idx.putDoc(namespace, d); err != nil {
			return err
		}
		if err := batch.Index(d.ID, textDoc{Content: d.Content}); err != nil {
			return fmt.Errorf("retrieval/bbh: bleve index %s: %w", d.ID, err)
		}
		if len(d.Vector) > 0 {
			if old, ok := sh.graph.Lookup(d.ID); ok {
				if !sameVector(old, d.Vector) {
					graphDirty = true
					rebuildGraph = true
					continue
				}
			} else {
				sh.graph.Add(hnsw.MakeNode(d.ID, append([]float32(nil), d.Vector...)))
				graphDirty = true
			}
		} else if _, ok := sh.graph.Lookup(d.ID); ok {
			graphDirty = true
			rebuildGraph = true
		}
	}
	if err := sh.text.Batch(batch); err != nil {
		return fmt.Errorf("retrieval/bbh: bleve batch: %w", err)
	}
	if graphDirty {
		if rebuildGraph {
			if err := idx.rebuildGraphLocked(namespace, sh); err != nil {
				return err
			}
		}
		sh.graphDirty = true
	}
	return nil
}

// Delete implements retrieval.Index.
func (idx *Index) Delete(ctx context.Context, namespace string, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	sh, err := idx.ensureShard(namespace)
	if err != nil {
		return err
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()

	graphDirty := false
	err = idx.db.Update(func(txn *badger.Txn) error {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if err := txn.Delete(docKey(namespace, id)); err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
				return err
			}
			if err := sh.text.Delete(id); err != nil {
				return fmt.Errorf("retrieval/bbh: bleve delete %s: %w", id, err)
			}
			if _, ok := sh.graph.Lookup(id); ok {
				graphDirty = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if graphDirty {
		if err := idx.rebuildGraphLocked(namespace, sh); err != nil {
			return err
		}
		sh.graphDirty = true
	}
	return nil
}

func validateVectorBatchLocked(sh *shard, docs []retrieval.Doc) error {
	dims := sh.graph.Dims()
	for _, d := range docs {
		if len(d.Vector) == 0 {
			continue
		}
		if dims == 0 {
			dims = len(d.Vector)
			continue
		}
		if dims != len(d.Vector) {
			return errdefs.Validationf("retrieval/bbh: vector dimension mismatch: graph=%d doc=%d", dims, len(d.Vector))
		}
	}
	return nil
}

func dedupeDocs(docs []retrieval.Doc) []retrieval.Doc {
	if len(docs) < 2 {
		return docs
	}
	last := make(map[string]int, len(docs))
	for i, d := range docs {
		last[d.ID] = i
	}
	out := make([]retrieval.Doc, 0, len(last))
	for i, d := range docs {
		if last[d.ID] == i {
			out = append(out, d)
		}
	}
	return out
}

func sameVector(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (idx *Index) rebuildGraphLocked(namespace string, sh *shard) error {
	docs, err := idx.listDocs(namespace, retrieval.Filter{})
	if err != nil {
		return err
	}
	graph := &hnsw.SavedGraph[string]{
		Graph: hnsw.NewGraph[string](),
		Path:  sh.hnswPath,
	}
	for _, d := range docs {
		if len(d.Vector) == 0 {
			continue
		}
		if dims := graph.Dims(); dims > 0 && dims != len(d.Vector) {
			return errdefs.Validationf("retrieval/bbh: vector dimension mismatch while rebuilding graph: graph=%d doc=%d", dims, len(d.Vector))
		}
		graph.Add(hnsw.MakeNode(d.ID, append([]float32(nil), d.Vector...)))
	}
	sh.graph = graph
	return nil
}

// Search implements retrieval.Index.
func (idx *Index) Search(ctx context.Context, namespace string, req retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start := time.Now()
	if err := idx.ensureOpen(); err != nil {
		return nil, err
	}
	hasText := strings.TrimSpace(req.QueryText) != ""
	hasVec := len(req.QueryVector) > 0
	hasSparse := len(req.SparseVec) > 0
	if !hasText && !hasVec {
		if hasSparse {
			return nil, errdefs.Validationf("retrieval/bbh: sparse search is not supported")
		}
		return nil, retrieval.ErrNoQuery
	}
	if hasSparse {
		return nil, errdefs.Validationf("retrieval/bbh: sparse search is not supported")
	}
	if req.TopK <= 0 {
		req.TopK = 10
	}
	sh, err := idx.ensureShard(namespace)
	if err != nil {
		return nil, err
	}
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	var hits []retrieval.Hit
	switch {
	case hasText && hasVec:
		laneReq := req
		laneReq.MinScore = 0
		textHits, err := idx.searchTextLocked(ctx, sh, laneReq)
		if err != nil {
			return nil, err
		}
		vectorHits, err := idx.searchVectorLocked(sh, laneReq)
		if err != nil {
			return nil, err
		}
		hits = scoring.RRF([][]retrieval.Hit{textHits, vectorHits}, scoring.DefaultRRFK)
	case hasText:
		hits, err = idx.searchTextLocked(ctx, sh, req)
		if err != nil {
			return nil, err
		}
	case hasVec:
		hits, err = idx.searchVectorLocked(sh, req)
		if err != nil {
			return nil, err
		}
	}
	if len(hits) > req.TopK {
		hits = hits[:req.TopK]
	}
	return &retrieval.SearchResponse{Hits: hits, Took: time.Since(start)}, nil
}

func (idx *Index) searchTextLocked(ctx context.Context, sh *shard, req retrieval.SearchRequest) ([]retrieval.Hit, error) {
	q := bleve.NewMatchQuery(req.QueryText)
	q.SetField(bleveContentField)
	size := idx.searchWindow(req.TopK, 0)
	out := make([]retrieval.Hit, 0, req.TopK)
	for offset := 0; ; offset += size {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sr := bleve.NewSearchRequestOptions(q, size, offset, false)
		res, err := sh.text.SearchInContext(ctx, sr)
		if err != nil {
			return nil, fmt.Errorf("retrieval/bbh: bleve search: %w", err)
		}
		if len(res.Hits) == 0 {
			break
		}
		for _, h := range res.Hits {
			d, ok, err := idx.getDoc(sh.namespace, h.ID)
			if err != nil {
				return nil, err
			}
			if !ok || !retrieval.DocMatchesFilter(d, req.Filter) {
				continue
			}
			if h.Score < req.MinScore {
				return out, nil
			}
			out = append(out, retrieval.Hit{
				Doc:   projectDoc(d, nil, true),
				Score: h.Score,
				Scores: map[string]float64{
					"bm25": h.Score,
				},
			})
			if len(out) >= req.TopK && req.TopK > 0 {
				return out, nil
			}
		}
		if len(res.Hits) < size || filterIsZero(req.Filter) {
			break
		}
	}
	return out, nil
}

func (idx *Index) searchVectorLocked(sh *shard, req retrieval.SearchRequest) ([]retrieval.Hit, error) {
	if dims := sh.graph.Dims(); dims > 0 && dims != len(req.QueryVector) {
		return nil, errdefs.Validationf("retrieval/bbh: query vector dimension mismatch: graph=%d query=%d", dims, len(req.QueryVector))
	}
	if !filterIsZero(req.Filter) {
		return idx.searchVectorFilteredScan(sh.namespace, req)
	}
	graphLen := sh.graph.Len()
	size := idx.searchWindow(req.TopK, graphLen)
	out := make([]retrieval.Hit, 0, req.TopK)
	nodes := sh.graph.Search(req.QueryVector, size)
	for _, n := range nodes {
		d, ok, err := idx.getDoc(sh.namespace, n.Key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		cos := scoring.CosineSim(req.QueryVector, n.Value)
		if cos < req.MinScore {
			continue
		}
		out = append(out, retrieval.Hit{
			Doc:      projectDoc(d, nil, true),
			Score:    cos,
			Distance: 1 - cos,
			Scores: map[string]float64{
				"cos": cos,
			},
		})
		if len(out) >= req.TopK && req.TopK > 0 {
			break
		}
	}
	return out, nil
}

func (idx *Index) searchVectorFilteredScan(namespace string, req retrieval.SearchRequest) ([]retrieval.Hit, error) {
	docs, err := idx.listDocs(namespace, req.Filter)
	if err != nil {
		return nil, err
	}
	out := make([]retrieval.Hit, 0, len(docs))
	for _, d := range docs {
		if len(d.Vector) != len(req.QueryVector) {
			continue
		}
		cos := scoring.CosineSim(req.QueryVector, d.Vector)
		if cos < req.MinScore {
			continue
		}
		out = append(out, retrieval.Hit{
			Doc:      projectDoc(d, nil, true),
			Score:    cos,
			Distance: 1 - cos,
			Scores: map[string]float64{
				"cos": cos,
			},
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].Doc.ID < out[j].Doc.ID
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > req.TopK {
		out = out[:req.TopK]
	}
	return out, nil
}

func filterIsZero(f retrieval.Filter) bool {
	return len(f.And) == 0 &&
		len(f.Or) == 0 &&
		f.Not == nil &&
		len(f.Eq) == 0 &&
		len(f.Neq) == 0 &&
		len(f.In) == 0 &&
		len(f.NotIn) == 0 &&
		len(f.Range) == 0 &&
		len(f.Exists) == 0 &&
		len(f.Missing) == 0 &&
		len(f.Match) == 0 &&
		len(f.Contains) == 0 &&
		len(f.IContains) == 0 &&
		len(f.ContainsAny) == 0 &&
		len(f.ContainsAll) == 0
}

func (idx *Index) searchWindow(topK int, max int) int {
	if topK <= 0 {
		topK = 10
	}
	n := topK * idx.cfg.SearchOverfetch
	if n < defaultMinSearchWindow {
		n = defaultMinSearchWindow
	}
	if max > 0 && n > max {
		n = max
	}
	if n <= 0 {
		return topK
	}
	return n
}

// List implements retrieval.Index.
func (idx *Index) List(ctx context.Context, namespace string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := idx.ensureOpen(); err != nil {
		return nil, err
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	if pageSize > defaultMaxListPageSize {
		pageSize = defaultMaxListPageSize
	}
	offset, err := retrieval.DecodeListPageTokenFor(req.PageToken, req)
	if err != nil {
		return nil, err
	}
	all, err := idx.listDocs(namespace, req.Filter)
	if err != nil {
		return nil, err
	}
	order := req.OrderBy
	if order == "" {
		order = retrieval.OrderByTimestampDesc
	}
	sort.SliceStable(all, func(i, j int) bool {
		switch order {
		case retrieval.OrderByTimestampAsc:
			if all[i].Timestamp.Equal(all[j].Timestamp) {
				return all[i].ID < all[j].ID
			}
			return all[i].Timestamp.Before(all[j].Timestamp)
		case retrieval.OrderByIDAsc:
			return all[i].ID < all[j].ID
		default:
			if all[i].Timestamp.Equal(all[j].Timestamp) {
				return all[i].ID > all[j].ID
			}
			return all[i].Timestamp.After(all[j].Timestamp)
		}
	})
	total := int64(len(all))
	if offset >= len(all) {
		return &retrieval.ListResponse{Items: []retrieval.Doc{}, Total: total}, nil
	}
	end := min(offset+pageSize, len(all))
	page := make([]retrieval.Doc, 0, end-offset)
	for _, d := range all[offset:end] {
		page = append(page, projectDoc(d, req.Project, req.WithVector))
	}
	next := ""
	if end < len(all) {
		next, err = retrieval.EncodeListPageTokenFor(end, req)
		if err != nil {
			return nil, err
		}
	}
	return &retrieval.ListResponse{Items: page, NextPageToken: next, Total: total}, nil
}

func (idx *Index) listDocs(namespace string, filter retrieval.Filter) ([]retrieval.Doc, error) {
	prefix := docPrefix(namespace)
	out := []retrieval.Doc{}
	err := idx.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{PrefetchValues: true})
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			var d retrieval.Doc
			if err := it.Item().Value(func(v []byte) error { return json.Unmarshal(v, &d) }); err != nil {
				return err
			}
			if retrieval.DocMatchesFilter(d, filter) {
				out = append(out, d)
			}
		}
		return nil
	})
	return out, err
}

// Count implements retrieval.Countable.
func (idx *Index) Count(ctx context.Context, namespace string, f retrieval.Filter) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := idx.ensureOpen(); err != nil {
		return 0, err
	}
	docs, err := idx.listDocs(namespace, f)
	if err != nil {
		return 0, err
	}
	return int64(len(docs)), nil
}

// DeleteByFilter implements retrieval.DeletableByFilter.
func (idx *Index) DeleteByFilter(ctx context.Context, namespace string, f retrieval.Filter) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if isEmptyFilter(f) {
		return 0, retrieval.ErrEmptyDeleteFilter
	}
	docs, err := idx.listDocs(namespace, f)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	if err := idx.Delete(ctx, namespace, ids); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

// Drop implements retrieval.Droppable.
func (idx *Index) Drop(ctx context.Context, namespace string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := idx.ensureOpen(); err != nil {
		return err
	}
	idx.shardMu.Lock()
	sh := idx.shards[namespace]
	delete(idx.shards, namespace)
	idx.shardMu.Unlock()
	var closeErr error
	if sh != nil {
		closeErr = sh.close()
	}
	if err := idx.db.DropPrefix(docPrefix(namespace), nsPrefix(namespace)); err != nil {
		return fmt.Errorf("retrieval/bbh: drop prefix: %w", err)
	}
	if sh != nil {
		return errors.Join(closeErr, os.RemoveAll(sh.bleveDir), removeIfExists(sh.hnswPath))
	}
	token := safeToken(namespace)
	return errors.Join(
		os.RemoveAll(filepath.Join(idx.root, bleveDir, token)),
		removeIfExists(filepath.Join(idx.root, hnswDir, token+hnswGraphExt)),
	)
}

// Iterate implements retrieval.Iterable.
func (idx *Index) Iterate(ctx context.Context, namespace string, cursor string, batch int) ([]retrieval.Doc, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if batch <= 0 {
		batch = 100
	}
	docs, err := idx.listDocs(namespace, retrieval.Filter{})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	start := 0
	if cursor != "" {
		start = sort.Search(len(docs), func(i int) bool { return docs[i].ID > cursor })
	}
	if start >= len(docs) {
		return nil, "", nil
	}
	end := min(start+batch, len(docs))
	next := ""
	if end < len(docs) {
		next = docs[end-1].ID
	}
	return docs[start:end], next, nil
}

func projectDoc(d retrieval.Doc, project []string, withVector bool) retrieval.Doc {
	if !withVector {
		d.Vector = nil
		d.SparseVector = nil
	}
	if len(project) == 0 {
		return d
	}
	keep := map[string]struct{}{}
	for _, p := range project {
		keep[p] = struct{}{}
	}
	out := retrieval.Doc{ID: d.ID}
	if _, ok := keep["content"]; ok {
		out.Content = d.Content
	}
	if _, ok := keep["timestamp"]; ok {
		out.Timestamp = d.Timestamp
	}
	if _, ok := keep["vector"]; ok && withVector {
		out.Vector = d.Vector
	}
	if _, ok := keep["sparse_vector"]; ok && withVector {
		out.SparseVector = d.SparseVector
	}
	if len(d.Metadata) > 0 {
		out.Metadata = map[string]any{}
		for _, p := range project {
			if strings.HasPrefix(p, "metadata.") {
				k := strings.TrimPrefix(p, "metadata.")
				if v, ok := d.Metadata[k]; ok {
					out.Metadata[k] = v
				}
			}
		}
		if _, ok := keep["metadata"]; ok {
			out.Metadata = d.Metadata
		}
		if len(out.Metadata) == 0 {
			out.Metadata = nil
		}
	}
	return out
}

func isEmptyFilter(f retrieval.Filter) bool {
	return f.Not == nil &&
		len(f.And) == 0 &&
		len(f.Or) == 0 &&
		len(f.Eq) == 0 &&
		len(f.Neq) == 0 &&
		len(f.In) == 0 &&
		len(f.NotIn) == 0 &&
		len(f.Range) == 0 &&
		len(f.Exists) == 0 &&
		len(f.Missing) == 0 &&
		len(f.Match) == 0 &&
		len(f.Contains) == 0 &&
		len(f.IContains) == 0 &&
		len(f.ContainsAny) == 0 &&
		len(f.ContainsAll) == 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func init() {
	// Ensure the default distance function remains exportable even if caller
	// code registers additional functions elsewhere.
	hnsw.RegisterDistanceFunc("cosine", hnsw.CosineDistance)
}
