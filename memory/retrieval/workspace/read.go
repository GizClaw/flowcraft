package workspace

import (
	"context"
	"sort"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/scoring"
	"github.com/GizClaw/flowcraft/memory/text/bm25"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Search runs a single-modality or hybrid retrieval against ns.
//
// Algorithm:
//
//  1. Snapshot the manifest and memtable under the namespace's
//     read lock so the rest of the search runs against a consistent
//     view (concurrent writers don't perturb the candidate set).
//  2. Iterate segments newest-first plus the in-memory memtable.
//     The memtable wins over any segment for an ID it stages, and
//     a newer segment's tombstone wins over an older segment's
//     content.
//  3. BM25 scoring uses a per-Search GLOBAL corpus aggregated from
//     every live (non-tombstoned, not-overridden) segment +
//     memtable doc. Each segment caches its [tokenize.Tokenizer]
//     output via [segmentReader.loadBM25]; Search folds those
//     tokens (plus freshly-tokenized memtable docs) into a single
//     [bm25.CorpusStats] before scoring. This matches the
//     reference [memory/retrieval/memory.Index] behaviour and the
//     BM25 protocol — IDF is corpus-relative, so a per-segment
//     corpus would make a doc's rank depend on which segment it
//     happens to live in. Cosine for QueryVector is
//     [scoring.CosineSim] over the doc's Vector field. Hybrid
//     fuses the per-modality rankings via [scoring.RRF].
//  4. Filter pushdown: the workspace backend evaluates the full
//     [retrieval.Filter] tree directly against [retrieval.Doc] via
//     [retrieval.DocMatchesFilter].
//  5. MinScore: applied only on single-modality scoring paths
//     (BM25 OR cosine). Hybrid RRF scores live on a different scale;
//     the [retrieval.SearchRequest.MinScore] contract forbids
//     applying it there.
//
// Empty namespaces (no manifest yet) return an empty SearchResponse
// rather than an error: matches the in-memory backend so the
// pipeline package can iterate retrievers without special-casing
// "not yet ingested".
func (idx *Index) Search(
	ctx context.Context,
	namespace string,
	req retrieval.SearchRequest,
) (*retrieval.SearchResponse, error) {
	if idx.closed.Load() {
		return nil, ErrClosed
	}
	start := idx.cfg.now()

	hasText := req.QueryText != ""
	hasVec := len(req.QueryVector) > 0
	hasSparse := len(req.SparseVec) > 0
	if !hasText && !hasVec {
		if hasSparse {
			return nil, errdefs.Validationf("workspace retrieval: sparse_vec search is not supported")
		}
		return nil, retrieval.ErrNoQuery
	}

	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if err := fenceCheck(st); err != nil {
		return nil, err
	}

	// Hold the namespace RLock for the entire Search body so the
	// snapshot's segment refs remain physically valid while we
	// open + loadDocs / loadBM25 them. Releasing the lock right
	// after the snapshot (the pre-fix pattern) let a concurrent
	// compactor swap the manifest AND RemoveAll the merged source
	// segment dirs while in-flight Search was iterating its now-
	// stale segment list, surfacing 'segment file not found' to
	// callers (issue #170). RLock vs Lock contention is minimal —
	// only compaction and writers on the SAME namespace are
	// blocked; concurrent Searches still parallelise.
	st.rwMu.RLock()
	defer st.rwMu.RUnlock()
	manifestSnap := st.manifest
	memSnap := snapshotMemtable(st.memtable)

	keywords := bm25.ExtractKeywords(req.QueryText, idx.cfg.tokenizer)
	queryVec := req.QueryVector

	// Phase 1: walk the snapshot newest-first, collecting one
	// liveDoc per surviving ID. Filter is NOT applied here — the
	// global BM25 corpus must reflect every live doc in the
	// namespace (Lucene/`sdk/retrieval/memory.Index` behaviour);
	// applying the filter pre-scoring would bake the filtered
	// subset into IDF and break ranking when the same query is
	// run with different filters.
	live := make(map[string]*liveDoc)
	deleted := make(map[string]struct{})

	for _, it := range memSnap {
		if it.op == walOpDelete {
			deleted[it.id] = struct{}{}
			delete(live, it.id)
			continue
		}
		if it.doc == nil {
			continue
		}
		ld := &liveDoc{doc: cloneDoc(*it.doc)}
		if hasText {
			ld.tokens = idx.cfg.tokenizer.Tokenize(it.doc.Content)
		}
		live[it.id] = ld
	}

	segs := append([]segmentRef(nil), manifestSnap.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].ID > segs[j].ID })

	for _, ref := range segs {
		seg, err := openSegmentReader(ctx, idx.ws, st.paths, ref)
		if err != nil {
			return nil, err
		}
		// A segment's tombstone for ID X means X was deleted
		// at the time of this segment's flush, so any older
		// segment must not surface X.
		for id := range seg.tombSet {
			if _, ok := live[id]; !ok {
				deleted[id] = struct{}{}
			}
		}

		if err := seg.loadDocs(ctx); err != nil {
			return nil, err
		}
		if hasText {
			if err := seg.loadBM25(ctx, idx.cfg.tokenizer); err != nil {
				return nil, err
			}
		}

		for i, d := range seg.docs {
			if _, ok := deleted[d.ID]; ok {
				continue
			}
			if _, ok := live[d.ID]; ok {
				// A fresher writer already claimed this ID; older
				// segments can't override the freshest copy.
				continue
			}
			ld := &liveDoc{doc: cloneDoc(d)}
			if hasText && seg.docTokens != nil {
				ld.tokens = seg.docTokens[i]
			}
			live[d.ID] = ld
		}
	}

	// Phase 2: build the per-Search global corpus. Memtable docs
	// and segment docs go through the same path (AddDocument), so
	// pre-flush ranks match post-flush ranks for the same doc set.
	var globalCorpus *bm25.CorpusStats
	if hasText {
		globalCorpus = bm25.NewCorpus()
		for _, ld := range live {
			if len(ld.tokens) > 0 {
				globalCorpus.AddDocument(ld.tokens)
			}
		}
	}

	// Phase 3: score against the global corpus and apply the
	// filter as a post-step. Zero-score docs are deliberately kept
	// (matches the in-memory backend contract — every filter-
	// passing doc is a candidate; MinScore / TopK trims later).
	merged := make(map[string]*partial, len(live))
	for id, ld := range live {
		if !retrieval.DocMatchesFilter(ld.doc, req.Filter) {
			continue
		}
		p := &partial{doc: ld.doc}
		if hasText && globalCorpus != nil && globalCorpus.DocCount > 0 && len(keywords) > 0 {
			p.bm = bm25.Score(ld.tokens, keywords, globalCorpus)
		}
		if hasVec && len(ld.doc.Vector) == len(queryVec) {
			p.cos = scoring.CosineSim(queryVec, ld.doc.Vector)
		}
		merged[id] = p
	}

	scoreds := make([]partial, 0, len(merged))
	for _, p := range merged {
		scoreds = append(scoreds, *p)
	}

	hits := rankAndProject(scoreds, hasText, hasVec, req.TopK, req.MinScore)
	return &retrieval.SearchResponse{Hits: hits, Took: idx.cfg.now().Sub(start)}, nil
}

// liveDoc is the per-Search snapshot of one doc that survived the
// memtable / segment / tombstone merge. tokens is non-nil only on
// the BM25 path; an empty tokens slice (content tokenizes to
// nothing) is preserved so the doc is still ranked as a zero-score
// candidate.
type liveDoc struct {
	doc    retrieval.Doc
	tokens []string
}

// partial holds the per-doc accumulated lane scores during a Search.
type partial struct {
	doc retrieval.Doc
	bm  float64
	cos float64
}

// rankAndProject sorts scoreds, applies MinScore on single-modality
// paths, fuses with [scoring.RRF] on hybrid, and trims to TopK.
func rankAndProject(scoreds []partial, hasText, hasVec bool, topK int, minScore float64) []retrieval.Hit {
	if topK <= 0 {
		topK = 10
	}

	if hasText && hasVec {
		// Build per-lane ranked Hit lists. scoring.RRF fuses ranks,
		// not scores, so "tie-broken by sibling score" is fine
		// here — what matters is the relative order within each
		// lane.
		bmHits := buildLaneHits(scoreds, func(p partial) float64 { return p.bm })
		vecHits := buildLaneHits(scoreds, func(p partial) float64 { return p.cos })
		fused := scoring.RRF([][]retrieval.Hit{bmHits, vecHits}, scoring.DefaultRRFK)
		// Decorate with per-lane scores for observability; RRF
		// returned new Hits with empty Scores maps.
		bmByID := make(map[string]float64, len(scoreds))
		cosByID := make(map[string]float64, len(scoreds))
		for _, s := range scoreds {
			bmByID[s.doc.ID] = s.bm
			cosByID[s.doc.ID] = s.cos
		}
		for i := range fused {
			if fused[i].Scores == nil {
				fused[i].Scores = make(map[string]float64, 3)
			}
			fused[i].Scores["bm25"] = bmByID[fused[i].Doc.ID]
			fused[i].Scores["cos"] = cosByID[fused[i].Doc.ID]
			fused[i].Scores["rrf"] = fused[i].Score
		}
		if len(fused) > topK {
			fused = fused[:topK]
		}
		return fused
	}

	hits := make([]retrieval.Hit, 0, len(scoreds))
	for _, s := range scoreds {
		var sc float64
		switch {
		case hasText:
			sc = s.bm
		case hasVec:
			sc = s.cos
		}
		if sc < minScore {
			continue
		}
		hits = append(hits, retrieval.Hit{
			Doc:    s.doc,
			Score:  sc,
			Scores: map[string]float64{"bm25": s.bm, "cos": s.cos},
		})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

// buildLaneHits projects scoreds onto one lane's score, sorts
// descending, and drops zero-score entries (RRF treats every member
// of its input as ranked, so a 0-score doc would be wrongly given
// a respectable rank). Returns []retrieval.Hit suitable as a lane
// input to [scoring.RRF].
func buildLaneHits(scoreds []partial, score func(partial) float64) []retrieval.Hit {
	hits := make([]retrieval.Hit, 0, len(scoreds))
	for _, s := range scoreds {
		v := score(s)
		if v <= 0 {
			continue
		}
		hits = append(hits, retrieval.Hit{Doc: s.doc, Score: v})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

// snapshotMemtable returns a copy of the current memtable items so
// the search loop can run without holding the namespace's read lock.
func snapshotMemtable(m *memtable) []memtableItem {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]memtableItem, len(m.items))
	copy(out, m.items)
	return out
}

// cloneDoc deep-copies the parts a Search hit should not share with
// the index's internal storage (slices and maps).
func cloneDoc(d retrieval.Doc) retrieval.Doc {
	out := d
	if d.Vector != nil {
		out.Vector = append([]float32(nil), d.Vector...)
	}
	if d.Metadata != nil {
		out.Metadata = make(map[string]any, len(d.Metadata))
		for k, v := range d.Metadata {
			out.Metadata[k] = v
		}
	}
	if d.SparseVector != nil {
		out.SparseVector = make(map[string]float32, len(d.SparseVector))
		for k, v := range d.SparseVector {
			out.SparseVector[k] = v
		}
	}
	return out
}

// List walks every segment + the memtable, applies the filter,
// orders, and pages. O(N_total) scan; intended for management-
// style flows (admin UIs, exports, periodic compaction audits)
// rather than the request-time hot path.
func (idx *Index) List(
	ctx context.Context,
	namespace string,
	req retrieval.ListRequest,
) (*retrieval.ListResponse, error) {
	if idx.closed.Load() {
		return nil, ErrClosed
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	caps := idx.Capabilities()
	if caps.MaxListPageSize > 0 && pageSize > caps.MaxListPageSize {
		pageSize = caps.MaxListPageSize
	}
	offset, err := retrieval.DecodeListPageTokenFor(req.PageToken, req)
	if err != nil {
		return nil, err
	}

	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if err := fenceCheck(st); err != nil {
		return nil, err
	}

	// Hold RLock through the segment scan so a concurrent compactor
	// cannot RemoveAll the segment dirs we are about to open
	// (issue #170). Pagination / filter / sort work post-scan
	// happens against in-memory data; the deferred unlock still
	// covers that, which is a minor over-extension we accept to
	// keep the code simple.
	st.rwMu.RLock()
	defer st.rwMu.RUnlock()
	manifestSnap := st.manifest
	memSnap := snapshotMemtable(st.memtable)

	order := req.OrderBy
	if order == "" {
		order = retrieval.OrderByTimestampDesc
	}
	limit := offset + pageSize
	if limit < pageSize {
		limit = pageSize
	}
	collector := boundedListCollector{order: order, limit: limit}
	var total int64
	if err := idx.scanLiveDocsNewestFirst(ctx, st, manifestSnap, memSnap, func(d retrieval.Doc) error {
		if !retrieval.DocMatchesFilter(d, req.Filter) {
			return nil
		}
		total++
		collector.add(d)
		return nil
	}); err != nil {
		return nil, err
	}
	all := collector.sorted()

	if int64(offset) >= total {
		return &retrieval.ListResponse{Items: []retrieval.Doc{}, Total: total}, nil
	}
	end := offset + pageSize
	if end < offset || end > len(all) {
		end = len(all)
	}
	page := append([]retrieval.Doc(nil), all[offset:end]...)
	for i := range page {
		page[i] = projectDoc(cloneDoc(page[i]), req.Project, req.WithVector)
	}
	next := ""
	if int64(end) < total {
		next, err = retrieval.EncodeListPageTokenFor(end, req)
		if err != nil {
			return nil, err
		}
	}
	return &retrieval.ListResponse{Items: page, NextPageToken: next, Total: total}, nil
}

type boundedListCollector struct {
	order retrieval.ListOrderBy
	limit int
	docs  []retrieval.Doc
}

func (c *boundedListCollector) add(d retrieval.Doc) {
	if c.limit <= 0 {
		return
	}
	if len(c.docs) < c.limit {
		c.docs = append(c.docs, d)
		return
	}
	worst := 0
	for i := 1; i < len(c.docs); i++ {
		if listDocLess(c.docs[worst], c.docs[i], c.order) {
			worst = i
		}
	}
	if listDocLess(d, c.docs[worst], c.order) {
		c.docs[worst] = d
	}
}

func (c *boundedListCollector) sorted() []retrieval.Doc {
	sort.SliceStable(c.docs, func(i, j int) bool {
		return listDocLess(c.docs[i], c.docs[j], c.order)
	})
	return c.docs
}

func listDocLess(a, b retrieval.Doc, order retrieval.ListOrderBy) bool {
	switch order {
	case retrieval.OrderByTimestampAsc:
		if a.Timestamp.Equal(b.Timestamp) {
			return a.ID < b.ID
		}
		return a.Timestamp.Before(b.Timestamp)
	case retrieval.OrderByIDAsc:
		return a.ID < b.ID
	default:
		if a.Timestamp.Equal(b.Timestamp) {
			return a.ID > b.ID
		}
		return a.Timestamp.After(b.Timestamp)
	}
}

func (idx *Index) scanLiveDocsNewestFirst(
	ctx context.Context,
	st *namespaceState,
	manifestSnap *manifest,
	memSnap []memtableItem,
	visit func(retrieval.Doc) error,
) error {
	seen := make(map[string]struct{}, len(memSnap))
	for _, it := range memSnap {
		if it.id == "" {
			continue
		}
		seen[it.id] = struct{}{}
		if it.op == walOpUpsert && it.doc != nil {
			if err := visit(*it.doc); err != nil {
				return err
			}
		}
	}
	if manifestSnap == nil {
		return nil
	}
	segs := append([]segmentRef(nil), manifestSnap.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].ID > segs[j].ID })
	for _, ref := range segs {
		seg, err := openSegmentReader(ctx, idx.ws, st.paths, ref)
		if err != nil {
			return err
		}
		for id := range seg.tombSet {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
			}
		}
		if err := seg.loadDocs(ctx); err != nil {
			return err
		}
		for _, d := range seg.docs {
			if _, ok := seen[d.ID]; ok {
				continue
			}
			seen[d.ID] = struct{}{}
			if err := visit(d); err != nil {
				return err
			}
		}
	}
	return nil
}

// Count implements retrieval.Countable.
func (idx *Index) Count(ctx context.Context, namespace string, f retrieval.Filter) (int64, error) {
	if idx.closed.Load() {
		return 0, ErrClosed
	}
	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return 0, err
	}
	if err := fenceCheck(st); err != nil {
		return 0, err
	}
	st.rwMu.RLock()
	defer st.rwMu.RUnlock()
	manifestSnap := st.manifest
	memSnap := snapshotMemtable(st.memtable)

	var total int64
	if err := idx.scanLiveDocsNewestFirst(ctx, st, manifestSnap, memSnap, func(d retrieval.Doc) error {
		if retrieval.DocMatchesFilter(d, f) {
			total++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return total, nil
}

// Get implements [retrieval.DocGetter]. Returns ok=false (no error)
// when the ID is unknown or has been tombstoned.
func (idx *Index) Get(ctx context.Context, namespace, id string) (retrieval.Doc, bool, error) {
	if idx.closed.Load() {
		return retrieval.Doc{}, false, ErrClosed
	}
	if id == "" {
		return retrieval.Doc{}, false, nil
	}
	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return retrieval.Doc{}, false, err
	}
	if err := fenceCheck(st); err != nil {
		return retrieval.Doc{}, false, err
	}
	// Hold RLock through segment open so compaction's RemoveAll
	// cannot race us into 'segment file not found' (issue #170).
	st.rwMu.RLock()
	defer st.rwMu.RUnlock()
	manifestSnap := st.manifest
	memSnap := snapshotMemtable(st.memtable)

	// Memtable wins; scan in reverse so the freshest staged op is
	// returned first.
	for i := len(memSnap) - 1; i >= 0; i-- {
		it := memSnap[i]
		if it.id != id {
			continue
		}
		if it.op == walOpDelete {
			return retrieval.Doc{}, false, nil
		}
		if it.doc != nil {
			return cloneDoc(*it.doc), true, nil
		}
	}

	segs := append([]segmentRef(nil), manifestSnap.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].ID > segs[j].ID })
	for _, ref := range segs {
		seg, err := openSegmentReader(ctx, idx.ws, st.paths, ref)
		if err != nil {
			return retrieval.Doc{}, false, err
		}
		if seg.isTombstoned(id) {
			return retrieval.Doc{}, false, nil
		}
		if err := seg.loadDocs(ctx); err != nil {
			return retrieval.Doc{}, false, err
		}
		if i, ok := seg.idIndex[id]; ok {
			return cloneDoc(seg.docs[i]), true, nil
		}
	}
	return retrieval.Doc{}, false, nil
}

// projectDoc applies [retrieval.ListRequest] projection: trims
// metadata to the named keys and optionally drops the vector.
func projectDoc(d retrieval.Doc, fields []string, withVector bool) retrieval.Doc {
	if !withVector {
		d.Vector = nil
	}
	if len(fields) == 0 {
		return d
	}
	keep := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		keep[f] = struct{}{}
	}
	if d.Metadata != nil {
		nm := make(map[string]any, len(keep))
		for k, v := range d.Metadata {
			if _, ok := keep[k]; ok {
				nm[k] = v
			}
		}
		d.Metadata = nm
	}
	return d
}

// SupportsFilter implements [retrieval.Filterable]. The workspace
// backend evaluates every operator natively against
// [retrieval.Doc], so we accept all filters.
func (idx *Index) SupportsFilter(_ retrieval.Filter) bool { return true }

// DeleteByFilter implements [retrieval.DeletableByFilter]. Walks
// every doc in the namespace, applies the filter, and issues a
// single Delete with the matching IDs. Empty filters are rejected
// with [retrieval.ErrEmptyDeleteFilter] to prevent accidental
// "delete everything" calls — same contract the in-memory backend
// honours.
//
// Implemented as List + Delete rather than a manifest-time
// physical purge so the writer's WAL/segment story stays
// linear: each tombstone goes through Upsert/Delete -> WAL ->
// memtable -> flush -> segment, exactly like a per-ID Delete.
// At the cost of one tombstone per matched doc this keeps
// crash recovery semantics uniform.
func (idx *Index) DeleteByFilter(ctx context.Context, namespace string, f retrieval.Filter) (int64, error) {
	if idx.closed.Load() {
		return 0, ErrClosed
	}
	if isEmptyFilter(f) {
		return 0, retrieval.ErrEmptyDeleteFilter
	}
	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return 0, err
	}
	if err := fenceCheck(st); err != nil {
		return 0, err
	}

	// Run the scan-to-collect-IDs phase under RLock so a concurrent
	// compactor cannot RemoveAll our snapshot's segment dirs mid-
	// scan (issue #170). The follow-up Delete acquires Lock for
	// the same namespace, so we MUST release RLock before invoking
	// it — defer is not safe here.
	matched, err := func() ([]string, error) {
		st.rwMu.RLock()
		defer st.rwMu.RUnlock()
		manifestSnap := st.manifest
		memSnap := snapshotMemtable(st.memtable)

		out := make([]string, 0)
		if err := idx.scanLiveDocsNewestFirst(ctx, st, manifestSnap, memSnap, func(d retrieval.Doc) error {
			if retrieval.DocMatchesFilter(d, f) {
				out = append(out, d.ID)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		return out, nil
	}()
	if err != nil {
		return 0, err
	}
	if len(matched) == 0 {
		return 0, nil
	}
	if err := idx.Delete(ctx, namespace, matched); err != nil {
		return 0, err
	}
	return int64(len(matched)), nil
}

// isEmptyFilter mirrors [retrieval/memory.isEmptyFilter]; an empty
// predicate must NOT silently delete every doc in the namespace.
func isEmptyFilter(f retrieval.Filter) bool {
	return len(f.And) == 0 && len(f.Or) == 0 && f.Not == nil &&
		len(f.Eq) == 0 && len(f.Neq) == 0 && len(f.In) == 0 && len(f.NotIn) == 0 &&
		len(f.Range) == 0 && len(f.Exists) == 0 && len(f.Missing) == 0 && len(f.Match) == 0 &&
		len(f.Contains) == 0 && len(f.IContains) == 0 && len(f.ContainsAny) == 0 && len(f.ContainsAll) == 0
}
