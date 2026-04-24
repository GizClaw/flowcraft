package retrieval

import (
	"context"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	rt "github.com/GizClaw/flowcraft/sdk/retrieval"
)

// RetrievalLayerRepo persists DerivedLayers in retrieval.Index. Layers
// share a per-dataset namespace (kb_<dataset>__layers); document-level
// and dataset-level layers coexist there with a "scope" metadata
// distinguishing them.
//
// Search filters strictly by metadata.layer so contract guarantee #3
// (queries never cross layers) holds even on backends that ignore
// the layer dimension.
type RetrievalLayerRepo struct {
	idx rt.Index
}

// NewLayerRepo wires an existing retrieval.Index.
func NewLayerRepo(idx rt.Index) *RetrievalLayerRepo {
	return &RetrievalLayerRepo{idx: idx}
}

// Put writes a layer doc. LayerDetail is rejected because chunks own
// detail content; LayerAbstract / LayerOverview are accepted.
func (r *RetrievalLayerRepo) Put(ctx context.Context, layer knowledge.DerivedLayer) error {
	if layer.DatasetID == "" {
		return errdefs.Validationf("knowledge/retrieval: dataset_id is required")
	}
	if layer.Layer != knowledge.LayerAbstract && layer.Layer != knowledge.LayerOverview {
		return errdefs.Validationf("knowledge/retrieval: only LayerAbstract / LayerOverview are persisted (got %q)", layer.Layer)
	}
	ns := layersNamespace(layer.DatasetID)
	scope := scopeDoc
	if layer.DocName == "" {
		scope = scopeDataset
	}
	id := layerID(layer.DocName, string(layer.Layer))
	doc := rt.Doc{
		ID:      id,
		Content: layer.Content,
		Vector:  layer.Vector,
		Metadata: map[string]any{
			mdDatasetID: layer.DatasetID,
			mdDocName:   layer.DocName,
			mdLayer:     string(layer.Layer),
			mdScope:     scope,
			mdSourceVer: layer.Sig.SourceVer,
			mdPromptSig: layer.Sig.PromptSig,
			mdEmbedSig:  layer.Sig.EmbedSig,
		},
	}
	if err := r.idx.Upsert(ctx, ns, []rt.Doc{doc}); err != nil {
		return fmt.Errorf("knowledge/retrieval: upsert layer %s/%s: %w", layer.DatasetID, layer.DocName, err)
	}
	return nil
}

// Get reads a single layer (docName == "" -> dataset-level). Returns
// (nil, nil) when the layer is missing.
func (r *RetrievalLayerRepo) Get(ctx context.Context, datasetID, docName string, layer knowledge.Layer) (*knowledge.DerivedLayer, error) {
	if datasetID == "" {
		return nil, errdefs.Validationf("knowledge/retrieval: dataset_id is required")
	}
	if layer != knowledge.LayerAbstract && layer != knowledge.LayerOverview {
		return nil, errdefs.Validationf("knowledge/retrieval: only LayerAbstract / LayerOverview are searchable (got %q)", layer)
	}
	ns := layersNamespace(datasetID)
	id := layerID(docName, string(layer))

	if g, ok := r.idx.(rt.DocGetter); ok {
		doc, found, err := g.Get(ctx, ns, id)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("knowledge/retrieval: get layer %s: %w", id, err)
		}
		if !found {
			return nil, nil
		}
		return docToLayer(datasetID, docName, layer, doc), nil
	}

	page, err := r.idx.List(ctx, ns, rt.ListRequest{
		Filter: rt.Filter{
			And: []rt.Filter{
				{Eq: map[string]any{mdLayer: string(layer)}},
				{Eq: map[string]any{mdDocName: docName}},
			},
		},
		PageSize:   1,
		WithVector: true,
	})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("knowledge/retrieval: list layer %s/%s: %w", ns, id, err)
	}
	if page == nil || len(page.Items) == 0 {
		return nil, nil
	}
	return docToLayer(datasetID, docName, layer, page.Items[0]), nil
}

// DeleteByDoc removes both layers for a single document.
func (r *RetrievalLayerRepo) DeleteByDoc(ctx context.Context, datasetID, docName string) error {
	if datasetID == "" || docName == "" {
		return errdefs.Validationf("knowledge/retrieval: dataset_id and doc_name are required")
	}
	ns := layersNamespace(datasetID)
	if d, ok := r.idx.(rt.DeletableByFilter); ok {
		_, err := d.DeleteByFilter(ctx, ns, rt.Filter{Eq: map[string]any{mdDocName: docName}})
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/retrieval: delete-by-filter %s: %w", ns, err)
		}
		return nil
	}
	ids := []string{
		layerID(docName, string(knowledge.LayerAbstract)),
		layerID(docName, string(knowledge.LayerOverview)),
	}
	if err := r.idx.Delete(ctx, ns, ids); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("knowledge/retrieval: delete %s: %w", ns, err)
	}
	return nil
}

// DeleteByDataset drops the dataset's layers namespace if the backend
// supports it; falls back to filter or list-based delete otherwise.
func (r *RetrievalLayerRepo) DeleteByDataset(ctx context.Context, datasetID string) error {
	if datasetID == "" {
		return errdefs.Validationf("knowledge/retrieval: dataset_id is required")
	}
	ns := layersNamespace(datasetID)
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
	repo := &RetrievalChunkRepo{idx: r.idx}
	return repo.deleteByList(ctx, ns, rt.Filter{Eq: map[string]any{mdDatasetID: datasetID}})
}

// Search performs layer-strict recall over q.DatasetIDs (fan-out).
//
// Mode:
//   - ModeBM25:   text-only recall.
//   - ModeVector: vector recall when q.Vector is non-empty.
//   - ModeHybrid: both lanes; the SearchEngine.Ranker fuses them.
//
// q.Layer is enforced via metadata.layer Eq filter so contract
// guarantee #3 (queries never cross layers) holds even when the
// underlying backend ignores the layer dimension.
func (r *RetrievalLayerRepo) Search(ctx context.Context, q knowledge.LayerQuery) ([]knowledge.Candidate, error) {
	if q.Layer != knowledge.LayerAbstract && q.Layer != knowledge.LayerOverview {
		return nil, errdefs.Validationf("knowledge/retrieval: only LayerAbstract / LayerOverview are searchable (got %q)", q.Layer)
	}
	mode := knowledge.ResolveMode(q.Mode)
	topK := q.TopK
	if topK <= 0 {
		topK = 5
	}
	if len(q.DatasetIDs) == 0 {
		return nil, nil
	}

	wantBM25 := (mode == knowledge.ModeBM25 || mode == knowledge.ModeHybrid) && q.Text != ""
	wantVector := (mode == knowledge.ModeVector || mode == knowledge.ModeHybrid) && len(q.Vector) > 0

	var out []knowledge.Candidate
	for _, datasetID := range q.DatasetIDs {
		if datasetID == "" {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ns := layersNamespace(datasetID)
		filter := rt.Filter{Eq: map[string]any{mdLayer: string(q.Layer)}}

		if wantBM25 {
			hits, err := r.searchLane(ctx, ns, rt.SearchRequest{QueryText: q.Text, Filter: filter, TopK: topK})
			if err != nil {
				return nil, err
			}
			for _, h := range hits {
				out = append(out, layerHitToCandidate(datasetID, "layer", q.Layer, h))
			}
		}
		if wantVector {
			hits, err := r.searchLane(ctx, ns, rt.SearchRequest{QueryVector: q.Vector, Filter: filter, TopK: topK})
			if err != nil {
				return nil, err
			}
			for _, h := range hits {
				out = append(out, layerHitToCandidate(datasetID, "layer-vector", q.Layer, h))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Hit.Score > out[j].Hit.Score })
	if topK > 0 && len(out) > topK*2 {
		out = out[:topK*2]
	}
	return out, nil
}

func (r *RetrievalLayerRepo) searchLane(ctx context.Context, ns string, req rt.SearchRequest) ([]rt.Hit, error) {
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

func layerHitToCandidate(datasetID, source string, layer knowledge.Layer, h rt.Hit) knowledge.Candidate {
	docName, _ := h.Doc.Metadata[mdDocName].(string)
	sig := knowledge.DerivedSig{
		SourceVer: metadataUint64(h.Doc.Metadata[mdSourceVer]),
		PromptSig: metadataString(h.Doc.Metadata[mdPromptSig]),
		EmbedSig:  metadataString(h.Doc.Metadata[mdEmbedSig]),
	}
	return knowledge.Candidate{
		Source: source,
		Hit: knowledge.Hit{
			DatasetID:  datasetID,
			DocName:    docName,
			Layer:      layer,
			Content:    h.Doc.Content,
			Score:      h.Score,
			ChunkIndex: -1,
			Sig:        sig,
			Metadata:   copyAnyMetadata(h.Doc.Metadata),
		},
	}
}

func docToLayer(datasetID, docName string, layer knowledge.Layer, doc rt.Doc) *knowledge.DerivedLayer {
	out := &knowledge.DerivedLayer{
		DatasetID: datasetID,
		DocName:   docName,
		Layer:     layer,
		Content:   doc.Content,
		Vector:    doc.Vector,
	}
	out.Sig = knowledge.DerivedSig{
		SourceVer: metadataUint64(doc.Metadata[mdSourceVer]),
		PromptSig: metadataString(doc.Metadata[mdPromptSig]),
		EmbedSig:  metadataString(doc.Metadata[mdEmbedSig]),
	}
	return out
}

// layerID encodes (docName, layer) as a stable Doc.ID. Dataset-level
// layers use docName=="" so the ID becomes "<layer>".
func layerID(docName, layer string) string {
	if docName == "" {
		return layer
	}
	return layer + "#" + docName
}

var _ knowledge.LayerRepo = (*RetrievalLayerRepo)(nil)
