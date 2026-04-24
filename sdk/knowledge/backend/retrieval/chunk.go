package retrieval

import (
	"context"
	"fmt"
	"sort"

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

// Replace atomically swaps every chunk for (datasetID, docName).
//
// Implementation: when the underlying Index satisfies DeletableByFilter
// we issue a single bulk delete, then upsert the new chunks. Otherwise
// we fall back to List + Delete by ID, which is O(N) in the document's
// chunk count.
//
// Atomicity is best-effort: if the upsert fails after the delete the
// document ends up empty rather than partially updated. Backends that
// support transactions can override this by implementing both
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
		return nil
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
	return nil
}

// DeleteByDoc removes every chunk for (datasetID, docName).
func (r *RetrievalChunkRepo) DeleteByDoc(ctx context.Context, datasetID, docName string) error {
	if datasetID == "" || docName == "" {
		return errdefs.Validationf("knowledge/retrieval: dataset_id and doc_name are required")
	}
	return r.deleteByDoc(ctx, chunksNamespace(datasetID), docName)
}

// DeleteByDataset drops the dataset's namespace when the backend
// supports it; otherwise it falls back to filter-driven deletes.
func (r *RetrievalChunkRepo) DeleteByDataset(ctx context.Context, datasetID string) error {
	if datasetID == "" {
		return errdefs.Validationf("knowledge/retrieval: dataset_id is required")
	}
	ns := chunksNamespace(datasetID)
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
		if ctx.Err() != nil {
			return nil, ctx.Err()
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

var _ knowledge.ChunkRepo = (*RetrievalChunkRepo)(nil)
