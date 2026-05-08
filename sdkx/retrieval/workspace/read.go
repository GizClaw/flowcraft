package workspace

import (
	"context"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/scoring"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
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
//  3. Per-segment scoring: BM25 for QueryText is computed by
//     [textsearch.BM25] against the segment-local
//     [textsearch.CorpusStats] (rebuilt on first load by
//     [segmentReader.loadBM25]); cosine for QueryVector is
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
	if !hasText && !hasVec {
		return nil, retrieval.ErrNoQuery
	}

	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}

	// Snapshot under the read lock so memtable + manifest are
	// mutually consistent. Concurrent writes that arrive after we
	// release see a fresh manifest; readers in flight see the
	// snapshot they started with.
	st.rwMu.RLock()
	manifestSnap := st.manifest
	memSnap := snapshotMemtable(st.memtable)
	st.rwMu.RUnlock()

	keywords := textsearch.ExtractKeywords(req.QueryText, idx.cfg.tokenizer)
	queryVec := req.QueryVector

	// merged tracks the best score per ID across segments + memtable.
	// Newest writer wins on ID collisions; the search loop only
	// records a doc the first time it sees the ID and skips later
	// (older) copies.
	merged := make(map[string]*partial)
	deleted := make(map[string]struct{})

	// Memtable first: it is the freshest writer.
	for _, it := range memSnap {
		if it.op == walOpDelete {
			deleted[it.id] = struct{}{}
			delete(merged, it.id)
			continue
		}
		if it.doc == nil {
			continue
		}
		if !retrieval.DocMatchesFilter(*it.doc, req.Filter) {
			continue
		}
		p := &partial{doc: cloneDoc(*it.doc)}
		if hasText && len(keywords) > 0 {
			p.bm = scoreMemtableBM25(it.doc.Content, keywords, idx.cfg.tokenizer)
		}
		if hasVec && len(it.doc.Vector) == len(queryVec) {
			p.cos = scoring.CosineSim(queryVec, it.doc.Vector)
		}
		merged[it.id] = p
	}

	// Newest segment first so a fresh tombstone overrides an old
	// content row.
	segs := append([]segmentRef(nil), manifestSnap.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].ID > segs[j].ID })

	for _, ref := range segs {
		seg, err := openSegmentReader(ctx, idx.ws, st.paths, ref)
		if err != nil {
			return nil, err
		}
		// Apply this segment's tombstones to the cumulative set
		// FIRST: a segment's tombstone for ID X means X was deleted
		// at the time of this segment's flush, so any older
		// segment must not surface X.
		for id := range seg.tombSet {
			if _, ok := merged[id]; ok {
				delete(merged, id)
			}
			deleted[id] = struct{}{}
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
			if _, ok := merged[d.ID]; ok {
				// A fresher copy already ranks the doc; segments
				// can't override the freshest writer.
				continue
			}
			if !retrieval.DocMatchesFilter(d, req.Filter) {
				continue
			}
			p := &partial{doc: cloneDoc(d)}
			if hasText && seg.corpus != nil && len(keywords) > 0 {
				p.bm = textsearch.BM25(seg.docTokens[i], keywords, seg.corpus)
			}
			if hasVec && len(d.Vector) == len(queryVec) {
				p.cos = scoring.CosineSim(queryVec, d.Vector)
			}
			// Skip docs that scored zero on every active lane:
			// they would only inflate the ranking with documents
			// the query never actually matched.
			if !hasContribution(p, hasText, hasVec) {
				continue
			}
			merged[d.ID] = p
		}
	}

	scoreds := make([]partial, 0, len(merged))
	for _, p := range merged {
		scoreds = append(scoreds, *p)
	}

	hits := rankAndProject(scoreds, hasText, hasVec, req.TopK, req.MinScore)
	return &retrieval.SearchResponse{Hits: hits, Took: idx.cfg.now().Sub(start)}, nil
}

// partial holds the per-doc accumulated lane scores during a Search.
type partial struct {
	doc retrieval.Doc
	bm  float64
	cos float64
}

// hasContribution returns true when the doc has a non-zero score on
// at least one active lane.
func hasContribution(p *partial, hasText, hasVec bool) bool {
	if hasText && p.bm > 0 {
		return true
	}
	if hasVec && p.cos > 0 {
		return true
	}
	return false
}

// scoreMemtableBM25 scores a single memtable doc against the query
// keywords using a synthetic 1-doc corpus. Numerically inferior to
// a "real" segment hit (because the corpus stats are tiny) but
// keeps memtable docs from being absent from results pre-flush;
// post-flush the segment-local scores dominate the ranking.
func scoreMemtableBM25(content string, keywords []string, tok textsearch.Tokenizer) float64 {
	tokens := tok.Tokenize(content)
	if len(tokens) == 0 {
		return 0
	}
	corpus := &textsearch.CorpusStats{
		DocCount:  1,
		AvgLength: float64(len(tokens)),
		DocFreq:   make(map[string]int, len(keywords)),
	}
	for _, k := range keywords {
		corpus.DocFreq[k] = 1
	}
	return textsearch.BM25(tokens, keywords, corpus)
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
	offset, err := retrieval.DecodeListPageToken(req.PageToken)
	if err != nil {
		return nil, err
	}

	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}

	st.rwMu.RLock()
	manifestSnap := st.manifest
	memSnap := snapshotMemtable(st.memtable)
	st.rwMu.RUnlock()

	deleted := make(map[string]struct{})
	docsByID := make(map[string]retrieval.Doc)

	// Memtable wins.
	for _, it := range memSnap {
		if it.op == walOpDelete {
			deleted[it.id] = struct{}{}
			continue
		}
		if it.doc != nil {
			docsByID[it.id] = *it.doc
		}
	}

	segs := append([]segmentRef(nil), manifestSnap.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].ID > segs[j].ID })
	for _, ref := range segs {
		seg, err := openSegmentReader(ctx, idx.ws, st.paths, ref)
		if err != nil {
			return nil, err
		}
		for id := range seg.tombSet {
			if _, ok := docsByID[id]; ok {
				delete(docsByID, id)
			}
			deleted[id] = struct{}{}
		}
		if err := seg.loadDocs(ctx); err != nil {
			return nil, err
		}
		for _, d := range seg.docs {
			if _, ok := deleted[d.ID]; ok {
				continue
			}
			if _, ok := docsByID[d.ID]; ok {
				continue
			}
			docsByID[d.ID] = d
		}
	}

	all := make([]retrieval.Doc, 0, len(docsByID))
	for _, d := range docsByID {
		if !retrieval.DocMatchesFilter(d, req.Filter) {
			continue
		}
		all = append(all, d)
	}

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
	end := offset + pageSize
	if end > len(all) {
		end = len(all)
	}
	page := append([]retrieval.Doc(nil), all[offset:end]...)
	for i := range page {
		page[i] = projectDoc(cloneDoc(page[i]), req.Project, req.WithVector)
	}
	next := ""
	if end < len(all) {
		next, err = retrieval.EncodeListPageToken(end)
		if err != nil {
			return nil, err
		}
	}
	return &retrieval.ListResponse{Items: page, NextPageToken: next, Total: total}, nil
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
	st.rwMu.RLock()
	manifestSnap := st.manifest
	memSnap := snapshotMemtable(st.memtable)
	st.rwMu.RUnlock()

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
