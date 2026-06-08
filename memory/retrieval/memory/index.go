package memory

import (
	"context"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	hybridparams "github.com/GizClaw/flowcraft/memory/retrieval/internal/hybrid"
	"github.com/GizClaw/flowcraft/memory/retrieval/scoring"
	"github.com/GizClaw/flowcraft/memory/text/bm25"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Index is an in-process retrieval.Index with BM25 (textsearch) + cosine vector scoring.
type Index struct {
	mu         sync.RWMutex
	tokenizer  tokenize.Tokenizer
	namespaces map[string]*ns
}

type ns struct {
	docs      map[string]retrieval.Doc
	corpus    *bm25.CorpusStats
	docTokens map[string][]string
}

// New returns an empty in-memory Index using CJKTokenizer for BM25.
func New() *Index {
	return &Index{
		tokenizer:  &tokenize.CJKBigram{},
		namespaces: make(map[string]*ns),
	}
}

// Capabilities implements retrieval.Index.
func (m *Index) Capabilities() retrieval.Capabilities {
	return retrieval.DefaultMemoryCapabilities()
}

// Close implements retrieval.Index.
func (m *Index) Close() error { return nil }

// Get implements retrieval.DocGetter.
func (m *Index) Get(_ context.Context, namespace, id string) (retrieval.Doc, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.namespaces[namespace]
	if !ok {
		return retrieval.Doc{}, false, nil
	}
	d, ok := n.docs[id]
	if !ok {
		return retrieval.Doc{}, false, nil
	}
	return cloneDoc(d), true, nil
}

// Upsert implements retrieval.Index.
func (m *Index) Upsert(_ context.Context, namespace string, docs []retrieval.Doc) error {
	if namespace == "" {
		return errdefs.Validationf("retrieval: namespace is required")
	}
	for _, d := range docs {
		if strings.TrimSpace(d.ID) == "" {
			return errdefs.Validationf("retrieval: doc id is required")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.nsLocked(namespace)
	for _, d := range docs {
		d = cloneDoc(d)
		if _, ok := n.docs[d.ID]; ok {
			if toks, ok := n.docTokens[d.ID]; ok {
				n.corpus.RemoveDocument(toks)
				delete(n.docTokens, d.ID)
			}
		}
		toks := m.tokenizer.Tokenize(d.Content)
		n.corpus.AddDocument(toks)
		n.docTokens[d.ID] = toks
		n.docs[d.ID] = d
	}
	return nil
}

// Delete implements retrieval.Index.
func (m *Index) Delete(_ context.Context, namespace string, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.namespaces[namespace]
	if !ok {
		return nil
	}
	for _, id := range ids {
		if _, ok := n.docs[id]; !ok {
			continue
		}
		if toks, ok := n.docTokens[id]; ok {
			n.corpus.RemoveDocument(toks)
			delete(n.docTokens, id)
		}
		delete(n.docs, id)
	}
	return nil
}

// DeleteByFilter implements retrieval.DeletableByFilter.
func (m *Index) DeleteByFilter(_ context.Context, namespace string, f retrieval.Filter) (int64, error) {
	if isEmptyFilter(f) {
		return 0, retrieval.ErrEmptyDeleteFilter
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.namespaces[namespace]
	if !ok {
		return 0, nil
	}
	var count int64
	for id, d := range n.docs {
		if !retrieval.DocMatchesFilter(d, f) {
			continue
		}
		if toks, ok := n.docTokens[id]; ok {
			n.corpus.RemoveDocument(toks)
			delete(n.docTokens, id)
		}
		delete(n.docs, id)
		count++
	}
	return count, nil
}

// Count implements retrieval.Countable.
func (m *Index) Count(_ context.Context, namespace string, f retrieval.Filter) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.namespaces[namespace]
	if !ok {
		return 0, nil
	}
	var total int64
	for _, d := range n.docs {
		if retrieval.DocMatchesFilter(d, f) {
			total++
		}
	}
	return total, nil
}

// Drop implements retrieval.Droppable.
func (m *Index) Drop(_ context.Context, namespace string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.namespaces, namespace)
	return nil
}

// Search implements retrieval.Index.
func (m *Index) Search(_ context.Context, namespace string, req retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	if req.TopK <= 0 {
		req.TopK = 10
	}
	start := time.Now()
	hasText := strings.TrimSpace(req.QueryText) != ""
	hasVec := len(req.QueryVector) > 0
	hasSparse := len(req.SparseVec) > 0
	if !hasText && !hasVec && !hasSparse {
		return nil, retrieval.ErrNoQuery
	}
	keywords := bm25.ExtractKeywords(req.QueryText, m.tokenizer)
	type scored struct {
		d      retrieval.Doc
		bm25   float64
		cos    float64
		sparse float64
	}
	m.mu.RLock()
	n, ok := m.namespaces[namespace]
	if !ok {
		m.mu.RUnlock()
		return &retrieval.SearchResponse{Took: time.Since(start)}, nil
	}
	corpus := n.corpus
	var out []scored
	for _, d := range n.docs {
		if !retrieval.DocMatchesFilter(d, req.Filter) {
			continue
		}
		var bm float64
		if hasText && corpus.DocCount > 0 && len(keywords) > 0 {
			bm = bm25.ScoreText(d.Content, keywords, corpus, m.tokenizer)
		}
		var cos float64
		if hasVec && len(d.Vector) > 0 && len(d.Vector) == len(req.QueryVector) {
			cos = scoring.CosineSim(d.Vector, req.QueryVector)
		}
		var sp float64
		if hasSparse && len(d.SparseVector) > 0 {
			sp = sparseDot(d.SparseVector, req.SparseVec)
		}
		out = append(out, scored{d: d, bm25: bm, cos: cos, sparse: sp})
	}
	m.mu.RUnlock()

	signals := retrieval.SearchSignals(req)
	if len(signals) > 1 {
		mode, err := hybridparams.NormalizeMode(req.HybridMode)
		if err != nil {
			return nil, err
		}
		buildLane := func(signal retrieval.SearchSignal, score func(scored) float64) []retrieval.Hit {
			hits := make([]retrieval.Hit, 0, len(out))
			for _, s := range out {
				v := score(s)
				if v <= 0 {
					continue
				}
				hits = append(hits, retrieval.Hit{Doc: cloneDoc(s.d), Score: v, Scores: map[string]float64{string(signal): v}})
			}
			sort.SliceStable(hits, func(i, j int) bool {
				if hits[i].Score == hits[j].Score {
					return hits[i].Doc.ID < hits[j].Doc.ID
				}
				return hits[i].Score > hits[j].Score
			})
			return hits
		}
		lanes := make(map[string][]retrieval.Hit, 3)
		var ranked [][]retrieval.Hit
		if hasText {
			lanes[string(retrieval.SearchSignalBM25)] = buildLane(retrieval.SearchSignalBM25, func(s scored) float64 { return s.bm25 })
			ranked = append(ranked, lanes[string(retrieval.SearchSignalBM25)])
		}
		if hasVec {
			lanes[string(retrieval.SearchSignalVector)] = buildLane(retrieval.SearchSignalVector, func(s scored) float64 { return s.cos })
			ranked = append(ranked, lanes[string(retrieval.SearchSignalVector)])
		}
		if hasSparse {
			lanes[string(retrieval.SearchSignalSparse)] = buildLane(retrieval.SearchSignalSparse, func(s scored) float64 { return s.sparse })
			ranked = append(ranked, lanes[string(retrieval.SearchSignalSparse)])
		}
		var hits []retrieval.Hit
		switch mode {
		case retrieval.HybridRRF:
			k, err := hybridparams.RRFK(req.HybridOptions)
			if err != nil {
				return nil, err
			}
			hits = scoring.RRF(ranked, k)
		case retrieval.HybridWeighted:
			weights, err := hybridparams.Weights(req.HybridOptions, signals)
			if err != nil {
				return nil, err
			}
			hits = scoring.RawWeightedFusion(lanes, weights)
		case retrieval.HybridConvex:
			weights, err := hybridparams.ConvexWeights(req.HybridOptions, signals)
			if err != nil {
				return nil, err
			}
			hits = scoring.ConvexFusion(lanes, weights)
		}
		bmByID := make(map[string]float64, len(out))
		cosByID := make(map[string]float64, len(out))
		sparseByID := make(map[string]float64, len(out))
		for _, s := range out {
			bmByID[s.d.ID] = s.bm25
			cosByID[s.d.ID] = s.cos
			sparseByID[s.d.ID] = s.sparse
		}
		for i := range hits {
			fused := hits[i].Score
			hits[i].Scores = map[string]float64{
				"bm25":   bmByID[hits[i].Doc.ID],
				"cos":    cosByID[hits[i].Doc.ID],
				"sparse": sparseByID[hits[i].Doc.ID],
			}
			hits[i].Scores[string(mode)] = fused
		}
		// SearchRequest.MinScore is intentionally NOT consulted on the
		// hybrid path: fused scores live on a different scale than
		// raw BM25/cosine/sparse, and applying the same threshold here would
		// silently change meaning depending on which modes were supplied.
		if len(hits) > req.TopK {
			hits = hits[:req.TopK]
		}
		return &retrieval.SearchResponse{Hits: hits, Took: time.Since(start)}, nil
	}

	if hasSparse && !hasText && !hasVec {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].sparse == out[j].sparse {
				return out[i].d.ID < out[j].d.ID
			}
			return out[i].sparse > out[j].sparse
		})
	} else if hasText {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].bm25 == out[j].bm25 {
				return out[i].d.ID < out[j].d.ID
			}
			return out[i].bm25 > out[j].bm25
		})
	} else if hasVec {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].cos == out[j].cos {
				return out[i].d.ID < out[j].d.ID
			}
			return out[i].cos > out[j].cos
		})
	}
	var hits []retrieval.Hit
	for _, s := range out {
		sc := s.bm25
		switch {
		case hasSparse && !hasText && !hasVec:
			sc = s.sparse
		case hasVec && !hasText:
			sc = s.cos
		}
		if sc < req.MinScore {
			continue
		}
		hits = append(hits, retrieval.Hit{
			Doc:    cloneDoc(s.d),
			Score:  sc,
			Scores: map[string]float64{"bm25": s.bm25, "cos": s.cos, "sparse": s.sparse},
		})
	}
	if len(hits) > req.TopK {
		hits = hits[:req.TopK]
	}
	return &retrieval.SearchResponse{Hits: hits, Took: time.Since(start)}, nil
}

// List implements retrieval.Index.
func (m *Index) List(_ context.Context, namespace string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	caps := m.Capabilities()
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	if caps.MaxListPageSize > 0 && pageSize > caps.MaxListPageSize {
		pageSize = caps.MaxListPageSize
	}
	offset, err := retrieval.DecodeListPageTokenFor(req.PageToken, req)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	n, ok := m.namespaces[namespace]
	if !ok {
		m.mu.RUnlock()
		return &retrieval.ListResponse{}, nil
	}
	var all []retrieval.Doc
	for _, d := range n.docs {
		if retrieval.DocMatchesFilter(d, req.Filter) {
			all = append(all, d)
		}
	}
	m.mu.RUnlock()

	order := req.OrderBy
	if order == "" {
		order = retrieval.OrderByTimestampDesc
	}
	sort.SliceStable(all, func(i, j int) bool {
		switch order {
		case retrieval.OrderByTimestampAsc:
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
	page := all[offset:end]
	for i := range page {
		page[i] = projectDoc(cloneDoc(page[i]), req.Project, req.WithVector)
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

// Iterate implements retrieval.Iterable.
func (m *Index) Iterate(_ context.Context, namespace string, cursor string, batch int) ([]retrieval.Doc, string, error) {
	if batch <= 0 {
		batch = 100
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.namespaces[namespace]
	if !ok {
		return nil, "", nil
	}
	ids := make([]string, 0, len(n.docs))
	for id := range n.docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	start := 0
	if cursor != "" {
		idx := sort.SearchStrings(ids, cursor)
		if idx < len(ids) && ids[idx] == cursor {
			start = idx + 1
		} else {
			start = idx
		}
	}
	end := min(start+batch, len(ids))
	var docs []retrieval.Doc
	for i := start; i < end; i++ {
		docs = append(docs, cloneDoc(n.docs[ids[i]]))
	}
	next := ""
	if end < len(ids) {
		next = ids[end-1]
	}
	return docs, next, nil
}

func (m *Index) nsLocked(name string) *ns {
	n, ok := m.namespaces[name]
	if !ok {
		n = &ns{
			docs:      make(map[string]retrieval.Doc),
			corpus:    bm25.NewCorpus(),
			docTokens: make(map[string][]string),
		}
		m.namespaces[name] = n
	}
	return n
}

func projectDoc(d retrieval.Doc, project []string, withVector bool) retrieval.Doc {
	if !withVector {
		d.Vector = nil
		d.SparseVector = nil
	}
	if len(project) == 0 {
		return d
	}
	if d.Metadata == nil {
		return d
	}
	md := make(map[string]any, len(project))
	for _, k := range project {
		if v, ok := d.Metadata[k]; ok {
			md[k] = v
		}
	}
	d.Metadata = md
	return d
}

func cloneDoc(d retrieval.Doc) retrieval.Doc {
	out := d
	if d.Metadata != nil {
		out.Metadata = make(map[string]any, len(d.Metadata))
		maps.Copy(out.Metadata, d.Metadata)
	}
	if len(d.Vector) > 0 {
		out.Vector = append([]float32(nil), d.Vector...)
	}
	if len(d.SparseVector) > 0 {
		out.SparseVector = make(map[string]float32, len(d.SparseVector))
		maps.Copy(out.SparseVector, d.SparseVector)
	}
	return out
}

func isEmptyFilter(f retrieval.Filter) bool {
	return len(f.And) == 0 && len(f.Or) == 0 && f.Not == nil &&
		len(f.Eq) == 0 && len(f.Neq) == 0 && len(f.In) == 0 && len(f.NotIn) == 0 &&
		len(f.Range) == 0 && len(f.Exists) == 0 && len(f.Missing) == 0 && len(f.Match) == 0 &&
		len(f.Contains) == 0 && len(f.IContains) == 0 && len(f.ContainsAny) == 0 && len(f.ContainsAll) == 0
}

func sparseDot(doc, q map[string]float32) float64 {
	if len(q) == 0 {
		return 0
	}
	var s float64
	for k, qv := range q {
		if dv, ok := doc[k]; ok {
			s += float64(dv * qv)
		}
	}
	return s
}
