package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	rt "github.com/GizClaw/flowcraft/sdk/retrieval"
)

// metadata keys used by the chunk schema; centralised so callers building
// custom Filters can reference the same constants.
const (
	mdDatasetID  = "dataset_id"
	mdDocName    = "doc_name"
	mdChunkIndex = "chunk_index"
	mdLayer      = "layer"
	mdScope      = "scope"
	mdSourceVer  = "source_ver"
	mdChunkerSig = "chunker_sig"
	mdPromptSig  = "prompt_sig"
	mdEmbedSig   = "embed_sig"

	scopeDoc     = "doc"
	scopeDataset = "dataset"
)

// RetrievalChunkRepo stores DerivedChunks in a retrieval.Index; one
// namespace per dataset (see package doc). The repo never owns the
// Index lifecycle: callers construct the Index, hand it in, and Close
// it themselves.
type RetrievalChunkRepo struct {
	idx rt.Index
}

// NewChunkRepo wires an existing retrieval.Index.
func NewChunkRepo(idx rt.Index) *RetrievalChunkRepo {
	return &RetrievalChunkRepo{idx: idx}
}

// Replace atomically swaps every chunk for (datasetID, docName), and
// keeps the doc-level virtual document in the __docs namespace in
// sync so SearchDocs can answer doc-level BM25 from the underlying
// retrieval.Index's native scorer (#134).
//
// Implementation: when the underlying Index satisfies DeletableByFilter
// we issue a single bulk delete, then upsert the new chunks. Otherwise
// we fall back to List + Delete by ID, which is O(N) in the document's
// chunk count. The doc-level upsert is a single retrieval.Doc per
// Replace call (O(1) in chunk count), so the doc-level update does
// not change the asymptotic cost.
//
// Atomicity is best-effort: if any of the three writes (chunks
// delete, chunks upsert, docs upsert) fails after a previous one
// succeeds, the doc ends up partially updated. This matches the
// existing fs and pre-#134 retrieval contract; backends that support
// transactions can override this by implementing both
// DeletableByFilter and an atomic Upsert.
func (r *RetrievalChunkRepo) Replace(ctx context.Context, datasetID, docName string, chunks []knowledge.DerivedChunk) error {
	if datasetID == "" || docName == "" {
		return errdefs.Validationf("knowledge/retrieval: dataset_id and doc_name are required")
	}
	ns := chunksNamespace(datasetID)

	if err := r.deleteByDoc(ctx, ns, docName); err != nil {
		return err
	}
	if len(chunks) == 0 {
		// Doc has no chunks anymore; drop it from the doc-level
		// namespace too so SearchDocs does not surface a stale
		// empty entry.
		return r.deleteDocLevel(ctx, datasetID, docName)
	}

	docs := make([]rt.Doc, len(chunks))
	for i, c := range chunks {
		docs[i] = rt.Doc{
			ID:      chunkID(docName, c.Index),
			Content: c.Content,
			Vector:  c.Vector,
			Metadata: map[string]any{
				mdDatasetID:  datasetID,
				mdDocName:    docName,
				mdChunkIndex: c.Index,
				mdLayer:      string(knowledge.LayerDetail),
				mdSourceVer:  c.Sig.SourceVer,
				mdChunkerSig: c.Sig.ChunkerSig,
				mdEmbedSig:   c.Sig.EmbedSig,
			},
		}
	}
	if err := r.idx.Upsert(ctx, ns, docs); err != nil {
		return fmt.Errorf("knowledge/retrieval: upsert %s/%s: %w", datasetID, docName, err)
	}
	return r.replaceDocLevel(ctx, datasetID, docName, chunks)
}

// replaceDocLevel rewrites a single (datasetID, docName) entry in the
// __docs namespace. The doc-level Content is the chunk Content
// concatenated in chunk-index order (newline-delimited); this is
// what feeds the retrieval.Index's BM25 scorer when SearchDocs hits
// the namespace.
//
// CHUNK OVERLAP CAVEAT: when the chunker emits overlapping chunks
// (ChunkOverlap > 0), the concatenated Content double-counts tokens
// in the overlap region, slightly inflating that doc's TF for those
// tokens. The inflation is uniform per overlap setting across every
// doc in the corpus, so BM25 *ranking* is unaffected — only absolute
// scores drift. Callers running doc-level eval against published
// baselines (BEIR / MS-MARCO / TREC) should configure their chunker
// with ChunkOverlap=0 to eliminate the offset entirely.
func (r *RetrievalChunkRepo) replaceDocLevel(ctx context.Context, datasetID, docName string, chunks []knowledge.DerivedChunk) error {
	sorted := make([]knowledge.DerivedChunk, len(chunks))
	copy(sorted, chunks)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	var sb strings.Builder
	for i, c := range sorted {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(c.Content)
	}
	doc := rt.Doc{
		ID:      docName,
		Content: sb.String(),
		Metadata: map[string]any{
			mdDatasetID: datasetID,
			mdDocName:   docName,
			mdLayer:     string(knowledge.LayerDetail),
			mdSourceVer: sorted[0].Sig.SourceVer,
		},
	}
	if err := r.idx.Upsert(ctx, docsNamespace(datasetID), []rt.Doc{doc}); err != nil {
		return fmt.Errorf("knowledge/retrieval: upsert doc %s/%s: %w", datasetID, docName, err)
	}
	return nil
}

// deleteDocLevel removes (datasetID, docName) from the __docs
// namespace. Backends that return NotFound on missing IDs are
// tolerated — this is also the path used when the doc never had a
// doc-level entry to begin with (e.g. legacy data ingested before
// #134, dropped via Replace([])).
func (r *RetrievalChunkRepo) deleteDocLevel(ctx context.Context, datasetID, docName string) error {
	if err := r.idx.Delete(ctx, docsNamespace(datasetID), []string{docName}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("knowledge/retrieval: delete doc %s/%s: %w", datasetID, docName, err)
	}
	return nil
}

// DeleteByDoc removes every chunk for (datasetID, docName) and the
// matching doc-level entry from the __docs namespace.
func (r *RetrievalChunkRepo) DeleteByDoc(ctx context.Context, datasetID, docName string) error {
	if datasetID == "" || docName == "" {
		return errdefs.Validationf("knowledge/retrieval: dataset_id and doc_name are required")
	}
	if err := r.deleteByDoc(ctx, chunksNamespace(datasetID), docName); err != nil {
		return err
	}
	return r.deleteDocLevel(ctx, datasetID, docName)
}

// DeleteByDataset drops the dataset's chunks AND doc-level namespaces
// when the backend supports it; otherwise it falls back to
// filter-driven deletes on both.
func (r *RetrievalChunkRepo) DeleteByDataset(ctx context.Context, datasetID string) error {
	if datasetID == "" {
		return errdefs.Validationf("knowledge/retrieval: dataset_id is required")
	}
	if err := r.deleteDatasetNamespace(ctx, datasetID, chunksNamespace(datasetID)); err != nil {
		return err
	}
	return r.deleteDatasetNamespace(ctx, datasetID, docsNamespace(datasetID))
}

// deleteDatasetNamespace clears every doc carrying the dataset_id
// from a single namespace using whichever capability the backend
// advertises.
func (r *RetrievalChunkRepo) deleteDatasetNamespace(ctx context.Context, datasetID, ns string) error {
	if d, ok := r.idx.(rt.Droppable); ok {
		if err := d.Drop(ctx, ns); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/retrieval: drop %s: %w", ns, err)
		}
		return nil
	}
	if d, ok := r.idx.(rt.DeletableByFilter); ok {
		if _, err := d.DeleteByFilter(ctx, ns, rt.Filter{Eq: map[string]any{mdDatasetID: datasetID}}); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/retrieval: delete-by-filter %s: %w", ns, err)
		}
		return nil
	}
	return r.deleteByList(ctx, ns, rt.Filter{Eq: map[string]any{mdDatasetID: datasetID}})
}

// Search fans out across q.DatasetIDs, calling Index.Search once per
// dataset namespace and merging the results. Hits keep their per-namespace
// score; the SearchEngine.Ranker is responsible for fusion.
//
// Mode handling:
//   - ModeBM25:   Filter only on dataset_id; Index uses its keyword backend.
//   - ModeVector: Requires q.Vector; sets SearchRequest.QueryVector.
//   - ModeHybrid: Sends both QueryText and QueryVector; backends that
//     implement Hybridable use SearchHybrid, others fall back to Search.
func (r *RetrievalChunkRepo) Search(ctx context.Context, q knowledge.ChunkQuery) ([]knowledge.Candidate, error) {
	mode := knowledge.ResolveMode(q.Mode)
	topK := q.TopK
	if topK <= 0 {
		topK = 5
	}
	if len(q.DatasetIDs) == 0 {
		return nil, nil
	}

	type plan struct {
		datasetID string
		ns        string
	}
	plans := make([]plan, 0, len(q.DatasetIDs))
	for _, id := range q.DatasetIDs {
		if id == "" {
			continue
		}
		plans = append(plans, plan{datasetID: id, ns: chunksNamespace(id)})
	}

	var out []knowledge.Candidate
	for _, p := range plans {
		if err := ctx.Err(); err != nil {
			return nil, errdefs.FromContext(err)
		}
		hits, err := r.recallOne(ctx, p.ns, p.datasetID, q, mode, topK)
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
	}
	return out, nil
}

// recallOne issues recall against a single namespace and projects the
// retrieval.Hit list into knowledge.Candidates.
func (r *RetrievalChunkRepo) recallOne(ctx context.Context, ns, datasetID string, q knowledge.ChunkQuery, mode knowledge.Mode, topK int) ([]knowledge.Candidate, error) {
	filter := rt.Filter{
		And: []rt.Filter{
			{Eq: map[string]any{mdDatasetID: datasetID}},
			{Eq: map[string]any{mdLayer: string(knowledge.LayerDetail)}},
		},
	}

	bm25Hits, err := r.searchOne(ctx, ns, q, filter, topK, mode, false)
	if err != nil {
		return nil, err
	}
	vectorHits, err := r.searchOne(ctx, ns, q, filter, topK, mode, true)
	if err != nil {
		return nil, err
	}

	out := make([]knowledge.Candidate, 0, len(bm25Hits)+len(vectorHits))
	for _, h := range bm25Hits {
		out = append(out, hitToChunkCandidate(datasetID, "bm25", h))
	}
	for _, h := range vectorHits {
		out = append(out, hitToChunkCandidate(datasetID, "vector", h))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Hit.Score > out[j].Hit.Score })
	if topK > 0 && len(out) > topK*2 {
		out = out[:topK*2]
	}
	return out, nil
}

// searchOne issues exactly one recall lane (BM25 or vector) when the
// requested mode includes it; returns nil otherwise.
func (r *RetrievalChunkRepo) searchOne(ctx context.Context, ns string, q knowledge.ChunkQuery, filter rt.Filter, topK int, mode knowledge.Mode, vectorLane bool) ([]rt.Hit, error) {
	wantBM25 := (mode == knowledge.ModeBM25 || mode == knowledge.ModeHybrid) && q.Text != ""
	wantVector := (mode == knowledge.ModeVector || mode == knowledge.ModeHybrid) && len(q.Vector) > 0
	if vectorLane && !wantVector {
		return nil, nil
	}
	if !vectorLane && !wantBM25 {
		return nil, nil
	}

	req := rt.SearchRequest{Filter: filter, TopK: topK}
	if vectorLane {
		req.QueryVector = q.Vector
	} else {
		req.QueryText = q.Text
	}
	resp, err := r.idx.Search(ctx, ns, req)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("knowledge/retrieval: search %s: %w", ns, err)
	}
	if resp == nil {
		return nil, nil
	}
	return resp.Hits, nil
}

// SearchDocs runs a doc-level BM25 query directly against the
// dataset's __docs namespace (see package doc and #134).
//
// Score semantics: scores come from the underlying retrieval.Index's
// native BM25 scorer evaluated against the doc-level corpus stats
// (N = number of documents in the dataset, avgdl = average doc
// length, df = doc-level document frequency). This is the same
// statistic regime Anserini uses for BEIR baselines and what
// FSChunkRepo's bespoke doc-level inverted index produces; it
// replaces the pre-#134 query-time chunk-level + sum-pool collapse,
// which cannot recover doc-level BM25 from chunk-level statistics
// (BM25 is nonlinear in TF / DocLength; see #134 for the math).
//
// Mode handling:
//   - ModeBM25:   primary path. Hits land at doc-level granularity.
//   - ModeVector / ModeHybrid: returns errdefs.NotAvailable. The
//     __docs namespace holds no per-doc vector (chunk vectors do
//     not compose into a doc vector without a model choice);
//     building doc-level vectors via mean-pool / late-chunking is
//     tracked as a follow-up. BEIR / MS-MARCO / TREC doc-level eval
//     suites are BM25-only, so this limitation does not affect
//     #134's acceptance criteria.
//
// Returned Hits have ChunkIndex = -1 and Layer = "" (doc-level
// results have no specific chunk); Content / Sig / Metadata are
// intentionally dropped — Content of the virtual doc is just the
// chunk concatenation and would mislead consumers expecting either
// the original source text or a specific chunk.
//
// Candidate.Source is always "bm25" on the v1 path.
//
// Doc-level results are deterministically ordered: primary by score
// (desc), tie-broken by (datasetID, docName) ascending so two
// scorers with identical doc-score distributions produce identical
// rankings across re-runs.
func (r *RetrievalChunkRepo) SearchDocs(ctx context.Context, q knowledge.ChunkQuery) ([]knowledge.Candidate, error) {
	mode := knowledge.ResolveMode(q.Mode)
	if mode != knowledge.ModeBM25 {
		return nil, errdefs.NotAvailablef(
			"knowledge/retrieval: SearchDocs supports ModeBM25 only in v1 (got %q); "+
				"doc-level vector/hybrid is tracked as a follow-up of #134",
			mode)
	}
	topK := q.TopK
	if topK <= 0 {
		topK = 5
	}
	if len(q.DatasetIDs) == 0 {
		return nil, nil
	}
	if q.Text == "" {
		return nil, nil
	}

	var out []knowledge.Candidate
	for _, ds := range q.DatasetIDs {
		if ds == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, errdefs.FromContext(err)
		}
		resp, err := r.idx.Search(ctx, docsNamespace(ds), rt.SearchRequest{
			QueryText: q.Text,
			TopK:      topK,
		})
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("knowledge/retrieval: search docs %s: %w", ds, err)
		}
		if resp == nil {
			continue
		}
		for _, h := range resp.Hits {
			// Zero-score hits leak through on some backends
			// (notably sdk/retrieval/memory returns every
			// filter-matched doc with Score=0 when no query
			// term hit); dropping them keeps doc-level
			// rankings honest. Mirrors what FSChunkRepo's
			// SearchDocs achieves by only emitting docs with
			// a posting hit.
			if h.Score <= 0 {
				continue
			}
			docName := metadataString(h.Doc.Metadata[mdDocName])
			if docName == "" {
				docName = h.Doc.ID
			}
			out = append(out, knowledge.Candidate{
				Source: "bm25",
				Hit: knowledge.Hit{
					DatasetID:  ds,
					DocName:    docName,
					Score:      h.Score,
					ChunkIndex: -1,
				},
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Hit.Score != out[j].Hit.Score {
			return out[i].Hit.Score > out[j].Hit.Score
		}
		if out[i].Hit.DatasetID != out[j].Hit.DatasetID {
			return out[i].Hit.DatasetID < out[j].Hit.DatasetID
		}
		return out[i].Hit.DocName < out[j].Hit.DocName
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// hitToChunkCandidate projects a retrieval.Hit into a Candidate carrying
// the chunk metadata back into the knowledge layer.
func hitToChunkCandidate(datasetID, source string, h rt.Hit) knowledge.Candidate {
	docName, _ := h.Doc.Metadata[mdDocName].(string)
	chunkIdx, _ := metadataInt(h.Doc.Metadata[mdChunkIndex])
	sig := knowledge.DerivedSig{
		SourceVer:  metadataUint64(h.Doc.Metadata[mdSourceVer]),
		ChunkerSig: metadataString(h.Doc.Metadata[mdChunkerSig]),
		EmbedSig:   metadataString(h.Doc.Metadata[mdEmbedSig]),
	}
	return knowledge.Candidate{
		Source: source,
		Hit: knowledge.Hit{
			DatasetID:  datasetID,
			DocName:    docName,
			Layer:      knowledge.LayerDetail,
			Content:    h.Doc.Content,
			Score:      h.Score,
			ChunkIndex: chunkIdx,
			Sig:        sig,
			Metadata:   copyAnyMetadata(h.Doc.Metadata),
		},
	}
}

// deleteByDoc removes every chunk for (ns, docName) using whichever
// capability the underlying Index advertises.
func (r *RetrievalChunkRepo) deleteByDoc(ctx context.Context, ns, docName string) error {
	if d, ok := r.idx.(rt.DeletableByFilter); ok {
		_, err := d.DeleteByFilter(ctx, ns, rt.Filter{Eq: map[string]any{mdDocName: docName}})
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/retrieval: delete-by-filter %s/%s: %w", ns, docName, err)
		}
		return nil
	}
	return r.deleteByList(ctx, ns, rt.Filter{Eq: map[string]any{mdDocName: docName}})
}

// deleteByList enumerates matching ids in pages and deletes them by id;
// fallback used when the Index does not support filter-based delete.
func (r *RetrievalChunkRepo) deleteByList(ctx context.Context, ns string, filter rt.Filter) error {
	pageToken := ""
	for {
		page, err := r.idx.List(ctx, ns, rt.ListRequest{Filter: filter, PageSize: 200, PageToken: pageToken})
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("knowledge/retrieval: list %s: %w", ns, err)
		}
		if page == nil || len(page.Items) == 0 {
			return nil
		}
		ids := make([]string, len(page.Items))
		for i, item := range page.Items {
			ids[i] = item.ID
		}
		if err := r.idx.Delete(ctx, ns, ids); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/retrieval: delete %s: %w", ns, err)
		}
		if page.NextPageToken == "" {
			return nil
		}
		pageToken = page.NextPageToken
	}
}

// chunkID is the ID written into retrieval.Doc; deterministic so
// re-Upserts overwrite cleanly. Format: "<doc>#<idx>".
func chunkID(docName string, idx int) string {
	return docName + "#" + itoa(idx)
}

// itoa avoids strconv to keep the package import surface minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// metadataInt coerces an arbitrary JSON-friendly metadata value into an int.
// Missing or non-numeric values produce -1 and false.
func metadataInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float32:
		return int(x), true
	case float64:
		return int(x), true
	case uint64:
		return int(x), true
	}
	return -1, false
}

// metadataUint64 is the uint64 variant used for SourceVer.
func metadataUint64(v any) uint64 {
	switch x := v.(type) {
	case uint64:
		return x
	case int64:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case int:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case float64:
		if x < 0 {
			return 0
		}
		return uint64(x)
	}
	return 0
}

// metadataString coerces a metadata value to string; missing -> "".
func metadataString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// copyAnyMetadata returns a shallow copy so backend mutations do not
// surface into hit consumers.
func copyAnyMetadata(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var (
	_ knowledge.ChunkRepo        = (*RetrievalChunkRepo)(nil)
	_ knowledge.DocLevelSearcher = (*RetrievalChunkRepo)(nil)
)
