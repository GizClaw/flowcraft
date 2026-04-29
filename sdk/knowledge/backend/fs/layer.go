package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// docLayersFile is the per-document DerivedSig sidecar schema.
type docLayersFile struct {
	Version int                                      `json:"version"`
	Sigs    map[knowledge.Layer]knowledge.DerivedSig `json:"sigs,omitempty"`
}

// datasetLayersFileSchema is the dataset-level DerivedSig sidecar schema.
type datasetLayersFileSchema struct {
	Version int                                      `json:"version"`
	Sigs    map[knowledge.Layer]knowledge.DerivedSig `json:"sigs,omitempty"`
}

// FSLayerRepo persists DerivedLayers as human-readable sidecars (Q7=A).
//
// Layout per document:
//
//	<doc>.abstract        L0 layer text
//	<doc>.abstract.vec    L0 layer embedding (binary, see vec.go)
//	<doc>.overview        L1 layer text
//	<doc>.overview.vec    L1 layer embedding
//	<doc>.layers.json     DerivedSig metadata for both layers
//
// Layout per dataset:
//
//	.abstract.md           dataset L0
//	.abstract.md.vec       dataset L0 embedding
//	.overview.md           dataset L1
//	.overview.md.vec       dataset L1 embedding
//	.dataset_layers.json   DerivedSig metadata for both
//
// Vectors are stored as separate sidecars so the human-readable text
// stays diffable in source control while embeddings remain binary. A
// missing or malformed .vec is a recoverable miss (vector lane skips
// the entry); the BM25 lane keeps working unchanged.
//
// Detail (L2) layers are not persisted by this repo: detail content
// already lives in chunks via FSChunkRepo. Put for LayerDetail returns a
// validation error.
type FSLayerRepo struct {
	ws        workspace.Workspace
	paths     pathBuilder
	tokenizer textsearch.Tokenizer

	mu sync.RWMutex
}

// NewLayerRepo wires an FSLayerRepo to ws under prefix.
func NewLayerRepo(ws workspace.Workspace, prefix string, tok textsearch.Tokenizer) *FSLayerRepo {
	return &FSLayerRepo{
		ws:        ws,
		paths:     newPathBuilder(prefix),
		tokenizer: tok,
	}
}

func (r *FSLayerRepo) resolveTokenizer() textsearch.Tokenizer {
	if r.tokenizer != nil {
		return r.tokenizer
	}
	return &textsearch.CJKTokenizer{}
}

// Put writes a layer text, refreshes its DerivedSig sidecar and
// persists the embedding vector when present.
//
//   - layer.DocName == "" denotes a dataset-level layer.
//   - layer.Layer must be LayerAbstract or LayerOverview.
//   - layer.Vector, when non-empty, is encoded via encodeVec and stored
//     in a co-located ".vec" sidecar; when empty, any pre-existing
//     sidecar is removed so the vector and text never drift apart.
func (r *FSLayerRepo) Put(ctx context.Context, layer knowledge.DerivedLayer) error {
	if layer.DatasetID == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id is required")
	}
	if layer.Layer != knowledge.LayerAbstract && layer.Layer != knowledge.LayerOverview {
		return errdefs.Validationf("knowledge/fs: only LayerAbstract / LayerOverview are persisted (got %q)", layer.Layer)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var textPath, vecPath string
	if layer.DocName == "" {
		textPath = r.paths.datasetLayerPath(layer.DatasetID, string(layer.Layer))
		vecPath = r.paths.datasetLayerVecPath(layer.DatasetID, string(layer.Layer))
	} else {
		textPath = r.paths.layerPath(layer.DatasetID, layer.DocName, string(layer.Layer))
		vecPath = r.paths.layerVecPath(layer.DatasetID, layer.DocName, string(layer.Layer))
	}

	if err := atomicWrite(ctx, r.ws, textPath, []byte(layer.Content)); err != nil {
		return err
	}
	if err := r.persistLayerVector(ctx, vecPath, layer.Vector); err != nil {
		return err
	}
	if layer.DocName == "" {
		return r.updateDatasetSigSidecar(ctx, layer.DatasetID, layer.Layer, layer.Sig)
	}
	return r.updateDocSigSidecar(ctx, layer.DatasetID, layer.DocName, layer.Layer, layer.Sig)
}

// persistLayerVector writes the encoded vector or removes the sidecar
// when the vector is empty. A missing sidecar on delete is treated as a
// no-op.
func (r *FSLayerRepo) persistLayerVector(ctx context.Context, vecPath string, vec []float32) error {
	if vecPath == "" {
		return nil
	}
	if len(vec) == 0 {
		if err := r.ws.Delete(ctx, vecPath); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/fs: delete layer vector %s: %w", vecPath, err)
		}
		return nil
	}
	return atomicWrite(ctx, r.ws, vecPath, encodeVec(vec))
}

// Get returns the layer for (datasetID, docName, layer); docName == ""
// reads the dataset-level layer. Returns (nil, nil) when missing.
func (r *FSLayerRepo) Get(ctx context.Context, datasetID, docName string, layer knowledge.Layer) (*knowledge.DerivedLayer, error) {
	if datasetID == "" {
		return nil, errdefs.Validationf("knowledge/fs: dataset_id is required")
	}
	if layer != knowledge.LayerAbstract && layer != knowledge.LayerOverview {
		return nil, errdefs.Validationf("knowledge/fs: only LayerAbstract / LayerOverview are persisted (got %q)", layer)
	}
	var path string
	if docName == "" {
		path = r.paths.datasetLayerPath(datasetID, string(layer))
	} else {
		path = r.paths.layerPath(datasetID, docName, string(layer))
	}
	data, err := r.ws.Read(ctx, path)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("knowledge/fs: read layer %s: %w", path, err)
	}
	out := &knowledge.DerivedLayer{
		DatasetID: datasetID,
		DocName:   docName,
		Layer:     layer,
		Content:   string(data),
	}
	if docName == "" {
		out.Sig = r.readDatasetSig(ctx, datasetID, layer)
		out.Vector = r.readLayerVector(ctx, r.paths.datasetLayerVecPath(datasetID, string(layer)))
	} else {
		out.Sig = r.readDocSig(ctx, datasetID, docName, layer)
		out.Vector = r.readLayerVector(ctx, r.paths.layerVecPath(datasetID, docName, string(layer)))
	}
	return out, nil
}

// readLayerVector loads and decodes the vec sidecar. Returns nil for any
// failure (missing file, decode error) so callers degrade to BM25 lane
// silently rather than failing the whole Search.
func (r *FSLayerRepo) readLayerVector(ctx context.Context, vecPath string) []float32 {
	if vecPath == "" {
		return nil
	}
	data, err := r.ws.Read(ctx, vecPath)
	if err != nil {
		return nil
	}
	vec, err := decodeVec(data)
	if err != nil {
		return nil
	}
	return vec
}

// DeleteByDoc removes per-document layer files, their vec sidecars and
// the sig sidecar. Missing files are ignored so the call is idempotent.
func (r *FSLayerRepo) DeleteByDoc(ctx context.Context, datasetID, docName string) error {
	if datasetID == "" || docName == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id and doc_name are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range []string{
		r.paths.layerPath(datasetID, docName, "L0"),
		r.paths.layerVecPath(datasetID, docName, "L0"),
		r.paths.layerPath(datasetID, docName, "L1"),
		r.paths.layerVecPath(datasetID, docName, "L1"),
		r.paths.docLayersPath(datasetID, docName),
	} {
		if p == "" {
			continue
		}
		if err := r.ws.Delete(ctx, p); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/fs: delete %s: %w", p, err)
		}
	}
	return nil
}

// DeleteByDataset removes the dataset-level layer files, their vec
// sidecars and the sig sidecar.
//
// Per-document layers must be cleaned via DeleteByDoc; this method does
// NOT enumerate documents to keep its cost predictable.
func (r *FSLayerRepo) DeleteByDataset(ctx context.Context, datasetID string) error {
	if datasetID == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range []string{
		r.paths.datasetLayerPath(datasetID, "L0"),
		r.paths.datasetLayerVecPath(datasetID, "L0"),
		r.paths.datasetLayerPath(datasetID, "L1"),
		r.paths.datasetLayerVecPath(datasetID, "L1"),
		r.paths.datasetLayersSigPath(datasetID),
	} {
		if p == "" {
			continue
		}
		if err := r.ws.Delete(ctx, p); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/fs: delete %s: %w", p, err)
		}
	}
	return nil
}

// Search performs a layer-tier scan with separate BM25 and vector
// lanes. The selected mode picks which lane runs:
//
//   - ModeBM25:   keyword scan only (vectors ignored).
//   - ModeVector: cosine scan only (text ignored); requires q.Vector
//     and a layer that has a stored vector sidecar.
//   - ModeHybrid (and the legacy default): both lanes run, results are
//     merged keyed by (dataset, docName), and the higher score wins so
//     downstream rankers see one Candidate per layer entity.
//
// Hits carry Layer == q.Layer (contract guarantee #3: queries never
// cross layers). When neither lane has anything to score, Search
// returns (nil, nil) so SearchEngine can keep fanning out without a
// hard failure.
func (r *FSLayerRepo) Search(ctx context.Context, q knowledge.LayerQuery) ([]knowledge.Candidate, error) {
	if q.Layer != knowledge.LayerAbstract && q.Layer != knowledge.LayerOverview {
		return nil, errdefs.Validationf("knowledge/fs: only LayerAbstract / LayerOverview are searchable (got %q)", q.Layer)
	}
	mode := knowledge.ResolveMode(q.Mode)
	wantBM25 := mode == knowledge.ModeBM25 || mode == knowledge.ModeHybrid
	wantVec := mode == knowledge.ModeVector || mode == knowledge.ModeHybrid

	tok := r.resolveTokenizer()
	var keywords []string
	if wantBM25 {
		keywords = textsearch.ExtractKeywords(q.Text, tok)
	}
	if len(keywords) == 0 {
		wantBM25 = false
	}
	if len(q.Vector) == 0 {
		wantVec = false
	}
	if !wantBM25 && !wantVec {
		return nil, nil
	}

	datasets, err := r.resolveDatasets(ctx, q.DatasetIDs)
	if err != nil {
		return nil, err
	}

	var out []knowledge.Candidate
	for _, ds := range datasets {
		if err := ctx.Err(); err != nil {
			return nil, errdefs.FromContext(err)
		}
		layers, err := r.collectLayerTexts(ctx, ds, q.Layer, wantVec)
		if err != nil {
			return nil, err
		}
		if len(layers) == 0 {
			continue
		}

		// scores[i] holds the best score across lanes for layers[i];
		// scored[i] tracks whether any lane produced a hit so we can
		// drop unscored entries before emitting Candidates.
		scores := make([]float64, len(layers))
		scored := make([]bool, len(layers))

		if wantBM25 {
			stats := textsearch.NewCorpusStats()
			toks := make([][]string, len(layers))
			for i, l := range layers {
				toks[i] = tok.Tokenize(l.Content)
				stats.AddDocument(toks[i])
			}
			for i := range layers {
				s := textsearch.BM25(toks[i], keywords, stats)
				if s <= 0 {
					continue
				}
				if !scored[i] || s > scores[i] {
					scores[i] = s
				}
				scored[i] = true
			}
		}

		if wantVec {
			for i, l := range layers {
				if len(l.Vector) == 0 {
					continue
				}
				s := knowledge.CosineSimilarity(q.Vector, l.Vector)
				if s <= 0 {
					continue
				}
				if !scored[i] || s > scores[i] {
					scores[i] = s
				}
				scored[i] = true
			}
		}

		for i, l := range layers {
			if !scored[i] {
				continue
			}
			out = append(out, knowledge.Candidate{
				Source: "layer",
				Hit: knowledge.Hit{
					DatasetID:  ds,
					DocName:    l.DocName,
					Layer:      q.Layer,
					Content:    l.Content,
					Score:      scores[i],
					ChunkIndex: -1,
					Sig:        l.Sig,
				},
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Hit.Score > out[j].Hit.Score })
	if q.TopK > 0 && len(out) > q.TopK*2 {
		out = out[:q.TopK*2]
	}
	return out, nil
}

// collectLayerTexts enumerates per-document layer texts plus the
// dataset-level layer (DocName == ""), pulling Sig metadata in
// lockstep. When withVectors is true, each entry's Vector field is
// populated from its co-located ".vec" sidecar (missing or unreadable
// sidecars yield a nil Vector, which the vector lane treats as a miss).
//
// The .vec suffix lookalikes are filtered explicitly: when listing
// "*.abstract" we must skip "<doc>.abstract.vec".
func (r *FSLayerRepo) collectLayerTexts(ctx context.Context, datasetID string, layer knowledge.Layer, withVectors bool) ([]knowledge.DerivedLayer, error) {
	entries, err := r.ws.List(ctx, r.paths.datasetDir(datasetID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("knowledge/fs: list %s: %w", datasetID, err)
	}
	var ext string
	switch layer {
	case knowledge.LayerAbstract:
		ext = ".abstract"
	case knowledge.LayerOverview:
		ext = ".overview"
	}
	var out []knowledge.DerivedLayer
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !endsWith(name, ext) || endsWith(name, vecSuffix) {
			continue
		}
		data, err := r.ws.Read(ctx, r.paths.datasetDir(datasetID)+"/"+name)
		if err != nil {
			continue
		}
		docName := name[:len(name)-len(ext)]
		entry := knowledge.DerivedLayer{
			DatasetID: datasetID,
			DocName:   docName,
			Layer:     layer,
			Content:   string(data),
			Sig:       r.readDocSig(ctx, datasetID, docName, layer),
		}
		if withVectors {
			entry.Vector = r.readLayerVector(ctx, r.paths.layerVecPath(datasetID, docName, string(layer)))
		}
		out = append(out, entry)
	}
	if data, err := r.ws.Read(ctx, r.paths.datasetLayerPath(datasetID, string(layer))); err == nil {
		entry := knowledge.DerivedLayer{
			DatasetID: datasetID,
			Layer:     layer,
			Content:   string(data),
			Sig:       r.readDatasetSig(ctx, datasetID, layer),
		}
		if withVectors {
			entry.Vector = r.readLayerVector(ctx, r.paths.datasetLayerVecPath(datasetID, string(layer)))
		}
		out = append(out, entry)
	}
	return out, nil
}

// resolveDatasets returns the explicit list when non-empty; otherwise
// enumerates every dataset directory under the prefix.
func (r *FSLayerRepo) resolveDatasets(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) > 0 {
		return ids, nil
	}
	entries, err := r.ws.List(ctx, r.paths.rootDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("knowledge/fs: list root: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || r.paths.isPrefixSelfEntry(e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

func (r *FSLayerRepo) updateDocSigSidecar(ctx context.Context, datasetID, docName string, layer knowledge.Layer, sig knowledge.DerivedSig) error {
	path := r.paths.docLayersPath(datasetID, docName)
	current := r.readDocLayersFile(ctx, datasetID, docName)
	if current.Sigs == nil {
		current.Sigs = make(map[knowledge.Layer]knowledge.DerivedSig)
	}
	current.Sigs[layer] = sig
	current.Version = chunksFileVersion // reuse the schema version for now
	payload, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("knowledge/fs: marshal layers sidecar: %w", err)
	}
	return atomicWrite(ctx, r.ws, path, payload)
}

func (r *FSLayerRepo) updateDatasetSigSidecar(ctx context.Context, datasetID string, layer knowledge.Layer, sig knowledge.DerivedSig) error {
	path := r.paths.datasetLayersSigPath(datasetID)
	current := r.readDatasetLayersFile(ctx, datasetID)
	if current.Sigs == nil {
		current.Sigs = make(map[knowledge.Layer]knowledge.DerivedSig)
	}
	current.Sigs[layer] = sig
	current.Version = chunksFileVersion
	payload, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("knowledge/fs: marshal dataset layers sidecar: %w", err)
	}
	return atomicWrite(ctx, r.ws, path, payload)
}

func (r *FSLayerRepo) readDocLayersFile(ctx context.Context, datasetID, docName string) docLayersFile {
	data, err := r.ws.Read(ctx, r.paths.docLayersPath(datasetID, docName))
	if err != nil {
		return docLayersFile{}
	}
	var f docLayersFile
	if err := json.Unmarshal(data, &f); err != nil {
		return docLayersFile{}
	}
	return f
}

func (r *FSLayerRepo) readDatasetLayersFile(ctx context.Context, datasetID string) datasetLayersFileSchema {
	data, err := r.ws.Read(ctx, r.paths.datasetLayersSigPath(datasetID))
	if err != nil {
		return datasetLayersFileSchema{}
	}
	var f datasetLayersFileSchema
	if err := json.Unmarshal(data, &f); err != nil {
		return datasetLayersFileSchema{}
	}
	return f
}

func (r *FSLayerRepo) readDocSig(ctx context.Context, datasetID, docName string, layer knowledge.Layer) knowledge.DerivedSig {
	f := r.readDocLayersFile(ctx, datasetID, docName)
	if f.Sigs == nil {
		return knowledge.DerivedSig{}
	}
	return f.Sigs[layer]
}

func (r *FSLayerRepo) readDatasetSig(ctx context.Context, datasetID string, layer knowledge.Layer) knowledge.DerivedSig {
	f := r.readDatasetLayersFile(ctx, datasetID)
	if f.Sigs == nil {
		return knowledge.DerivedSig{}
	}
	return f.Sigs[layer]
}

// endsWith reports whether s ends with suffix and is strictly longer
// (so a layer file named ".abstract" alone would not match).
func endsWith(s, suffix string) bool {
	return len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix
}

// Compile-time interface assertion.
var _ knowledge.LayerRepo = (*FSLayerRepo)(nil)
