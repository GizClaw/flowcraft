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
//	<doc>.abstract    L0 layer text
//	<doc>.overview    L1 layer text
//	<doc>.layers.json DerivedSig metadata for both layers
//
// Layout per dataset:
//
//	.abstract.md           dataset L0
//	.overview.md           dataset L1
//	.dataset_layers.json   DerivedSig metadata for both
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

// Put writes a layer text and refreshes its DerivedSig sidecar.
//
//   - layer.DocName == "" denotes a dataset-level layer.
//   - layer.Layer must be LayerAbstract or LayerOverview.
//   - layer.Vector is currently ignored (vectors for layers will land in
//     a follow-up; the on-disk schema reserves a slot for them).
func (r *FSLayerRepo) Put(ctx context.Context, layer knowledge.DerivedLayer) error {
	if layer.DatasetID == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id is required")
	}
	if layer.Layer != knowledge.LayerAbstract && layer.Layer != knowledge.LayerOverview {
		return errdefs.Validationf("knowledge/fs: only LayerAbstract / LayerOverview are persisted (got %q)", layer.Layer)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if layer.DocName == "" {
		path := r.paths.datasetLayerPath(layer.DatasetID, string(layer.Layer))
		if err := atomicWrite(ctx, r.ws, path, []byte(layer.Content)); err != nil {
			return err
		}
		return r.updateDatasetSigSidecar(ctx, layer.DatasetID, layer.Layer, layer.Sig)
	}

	path := r.paths.layerPath(layer.DatasetID, layer.DocName, string(layer.Layer))
	if err := atomicWrite(ctx, r.ws, path, []byte(layer.Content)); err != nil {
		return err
	}
	return r.updateDocSigSidecar(ctx, layer.DatasetID, layer.DocName, layer.Layer, layer.Sig)
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
	} else {
		out.Sig = r.readDocSig(ctx, datasetID, docName, layer)
	}
	return out, nil
}

// DeleteByDoc removes per-document layer files and the sig sidecar.
func (r *FSLayerRepo) DeleteByDoc(ctx context.Context, datasetID, docName string) error {
	if datasetID == "" || docName == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id and doc_name are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range []string{
		r.paths.layerPath(datasetID, docName, "L0"),
		r.paths.layerPath(datasetID, docName, "L1"),
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

// DeleteByDataset removes the dataset-level layer files and their sig sidecar.
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
		r.paths.datasetLayerPath(datasetID, "L1"),
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

// Search performs a layer-tier BM25 scan. Vector recall is not yet
// supported for layers; ModeVector returns an empty result without
// error so the SearchEngine can still fan out hybrid queries safely.
//
// Hits carry Layer == q.Layer (contract guarantee #3: queries never
// cross layers).
func (r *FSLayerRepo) Search(ctx context.Context, q knowledge.LayerQuery) ([]knowledge.Candidate, error) {
	if q.Layer != knowledge.LayerAbstract && q.Layer != knowledge.LayerOverview {
		return nil, errdefs.Validationf("knowledge/fs: only LayerAbstract / LayerOverview are searchable (got %q)", q.Layer)
	}
	mode := knowledge.ResolveMode(q.Mode)
	if mode == knowledge.ModeVector {
		return nil, nil
	}
	tok := r.resolveTokenizer()
	keywords := textsearch.ExtractKeywords(q.Text, tok)
	if len(keywords) == 0 {
		return nil, nil
	}
	datasets, err := r.resolveDatasets(ctx, q.DatasetIDs)
	if err != nil {
		return nil, err
	}

	var out []knowledge.Candidate
	for _, ds := range datasets {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		layers, err := r.collectLayerTexts(ctx, ds, q.Layer)
		if err != nil {
			return nil, err
		}
		if len(layers) == 0 {
			continue
		}
		stats := textsearch.NewCorpusStats()
		toks := make([][]string, len(layers))
		for i, l := range layers {
			toks[i] = tok.Tokenize(l.Content)
			stats.AddDocument(toks[i])
		}
		for i, l := range layers {
			score := textsearch.BM25(toks[i], keywords, stats)
			if q.Mode == knowledge.ModeBM25 && score <= 0 {
				continue
			}
			out = append(out, knowledge.Candidate{
				Source: "layer",
				Hit: knowledge.Hit{
					DatasetID:  ds,
					DocName:    l.DocName,
					Layer:      q.Layer,
					Content:    l.Content,
					Score:      score,
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
// dataset-level layer (DocName == ""), pulling Sig metadata in lockstep.
func (r *FSLayerRepo) collectLayerTexts(ctx context.Context, datasetID string, layer knowledge.Layer) ([]knowledge.DerivedLayer, error) {
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
		if !endsWith(name, ext) {
			continue
		}
		data, err := r.ws.Read(ctx, r.paths.datasetDir(datasetID)+"/"+name)
		if err != nil {
			continue
		}
		docName := name[:len(name)-len(ext)]
		out = append(out, knowledge.DerivedLayer{
			DatasetID: datasetID,
			DocName:   docName,
			Layer:     layer,
			Content:   string(data),
			Sig:       r.readDocSig(ctx, datasetID, docName, layer),
		})
	}
	if data, err := r.ws.Read(ctx, r.paths.datasetLayerPath(datasetID, string(layer))); err == nil {
		out = append(out, knowledge.DerivedLayer{
			DatasetID: datasetID,
			Layer:     layer,
			Content:   string(data),
			Sig:       r.readDatasetSig(ctx, datasetID, layer),
		})
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
