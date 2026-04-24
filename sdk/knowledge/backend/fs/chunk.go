package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// chunksFileVersion tags the on-disk chunks.json schema. Bumped only on
// breaking layout changes.
const chunksFileVersion = 1

// chunksFile is the dataset-level on-disk schema. Stored verbatim as JSON
// at <prefix>/<dataset>/.chunks.json via atomicWrite.
type chunksFile struct {
	Version int                       `json:"version"`
	Chunks  []knowledge.DerivedChunk  `json:"chunks"`
}

// chunkPosting is a per-term occurrence inside the in-memory inverted index.
type chunkPosting struct {
	chunkIdx int // index into datasetState.chunks
	tf       int
	dl       int
}

// datasetState keeps the live inverted index plus vector map for one dataset.
//
// Replace and Search synchronise on the parent FSChunkRepo's mu; the
// state itself never holds its own lock. This avoids fine-grained
// locking races and keeps reasoning simple.
type datasetState struct {
	chunks   []knowledge.DerivedChunk
	stats    *textsearch.CorpusStats
	inverted map[string][]chunkPosting
}

// newDatasetState builds an empty state with corpus stats initialised.
func newDatasetState() *datasetState {
	return &datasetState{
		stats:    textsearch.NewCorpusStats(),
		inverted: make(map[string][]chunkPosting),
	}
}

// FSChunkRepo persists DerivedChunks per dataset. Each dataset has one
// .chunks.json file and an in-memory inverted index built lazily on
// first access (or on Load).
//
// Concurrency: every public method is safe for concurrent use; the
// repo's RWMutex protects the state map and each datasetState.
type FSChunkRepo struct {
	ws        workspace.Workspace
	paths     pathBuilder
	tokenizer textsearch.Tokenizer

	mu     sync.RWMutex
	states map[string]*datasetState // datasetID -> live state
}

// NewChunkRepo constructs an FSChunkRepo. Tokenizer is auto-detected from
// the first content seen when nil; explicit override wins.
func NewChunkRepo(ws workspace.Workspace, prefix string, tok textsearch.Tokenizer) *FSChunkRepo {
	return &FSChunkRepo{
		ws:        ws,
		paths:     newPathBuilder(prefix),
		tokenizer: tok,
		states:    make(map[string]*datasetState),
	}
}

// resolveTokenizer returns the configured tokenizer or a CJK default.
func (r *FSChunkRepo) resolveTokenizer() textsearch.Tokenizer {
	if r.tokenizer != nil {
		return r.tokenizer
	}
	return &textsearch.CJKTokenizer{}
}

// Load rehydrates state for every dataset under the prefix. Idempotent;
// safe to call on startup. Failures for individual datasets are
// collected and returned as a joined error.
func (r *FSChunkRepo) Load(ctx context.Context) error {
	entries, err := r.ws.List(ctx, r.paths.rootDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("knowledge/fs: list root: %w", err)
	}
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() || r.paths.isPrefixSelfEntry(e.Name()) {
			continue
		}
		if err := r.loadDataset(ctx, e.Name()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// loadDataset reads one dataset's .chunks.json and rebuilds its index.
func (r *FSChunkRepo) loadDataset(ctx context.Context, datasetID string) error {
	data, err := r.ws.Read(ctx, r.paths.chunksPath(datasetID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("knowledge/fs: read chunks %s: %w", datasetID, err)
	}
	var file chunksFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("knowledge/fs: parse chunks %s: %w", datasetID, err)
	}
	state := newDatasetState()
	tok := r.resolveTokenizer()
	for _, c := range file.Chunks {
		idx := len(state.chunks)
		state.chunks = append(state.chunks, c)
		tokens := tok.Tokenize(c.Content)
		state.stats.AddDocument(tokens)
		addToInverted(state.inverted, idx, tokens)
	}
	r.mu.Lock()
	r.states[datasetID] = state
	r.mu.Unlock()
	return nil
}

// Replace atomically swaps every chunk for (datasetID, docName). The
// in-memory index is rebuilt from the merged chunk set and the dataset
// chunks.json is written atomically.
func (r *FSChunkRepo) Replace(ctx context.Context, datasetID, docName string, chunks []knowledge.DerivedChunk) error {
	if datasetID == "" || docName == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id and doc_name are required")
	}
	r.mu.Lock()
	state, ok := r.states[datasetID]
	if !ok {
		state = newDatasetState()
	}
	merged := make([]knowledge.DerivedChunk, 0, len(state.chunks)+len(chunks))
	for _, c := range state.chunks {
		if c.DocName == docName {
			continue
		}
		merged = append(merged, c)
	}
	for _, c := range chunks {
		c.DatasetID = datasetID
		c.DocName = docName
		merged = append(merged, c)
	}
	r.mu.Unlock()

	if err := r.persistDataset(ctx, datasetID, merged); err != nil {
		return err
	}
	r.installState(datasetID, merged)
	return nil
}

// DeleteByDoc removes every chunk for (datasetID, docName).
func (r *FSChunkRepo) DeleteByDoc(ctx context.Context, datasetID, docName string) error {
	if datasetID == "" || docName == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id and doc_name are required")
	}
	r.mu.RLock()
	state, ok := r.states[datasetID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	merged := make([]knowledge.DerivedChunk, 0, len(state.chunks))
	for _, c := range state.chunks {
		if c.DocName != docName {
			merged = append(merged, c)
		}
	}
	if err := r.persistDataset(ctx, datasetID, merged); err != nil {
		return err
	}
	r.installState(datasetID, merged)
	return nil
}

// DeleteByDataset removes every chunk for the dataset and the chunks.json file.
func (r *FSChunkRepo) DeleteByDataset(ctx context.Context, datasetID string) error {
	if datasetID == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id is required")
	}
	if err := r.ws.Delete(ctx, r.paths.chunksPath(datasetID)); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("knowledge/fs: delete chunks %s: %w", datasetID, err)
	}
	r.mu.Lock()
	delete(r.states, datasetID)
	r.mu.Unlock()
	return nil
}

// persistDataset serialises chunks to <prefix>/<dataset>/.chunks.json
// using atomicWrite. An empty chunks slice deletes the file.
func (r *FSChunkRepo) persistDataset(ctx context.Context, datasetID string, chunks []knowledge.DerivedChunk) error {
	path := r.paths.chunksPath(datasetID)
	if len(chunks) == 0 {
		if err := r.ws.Delete(ctx, path); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/fs: delete chunks %s: %w", datasetID, err)
		}
		return nil
	}
	payload, err := json.Marshal(chunksFile{Version: chunksFileVersion, Chunks: chunks})
	if err != nil {
		return fmt.Errorf("knowledge/fs: marshal chunks %s: %w", datasetID, err)
	}
	return atomicWrite(ctx, r.ws, path, payload)
}

// installState rebuilds the live in-memory state from the canonical
// chunks slice (the just-persisted version).
func (r *FSChunkRepo) installState(datasetID string, chunks []knowledge.DerivedChunk) {
	tok := r.resolveTokenizer()
	state := newDatasetState()
	for _, c := range chunks {
		idx := len(state.chunks)
		state.chunks = append(state.chunks, c)
		tokens := tok.Tokenize(c.Content)
		state.stats.AddDocument(tokens)
		addToInverted(state.inverted, idx, tokens)
	}
	r.mu.Lock()
	r.states[datasetID] = state
	r.mu.Unlock()
}

// Search runs the requested mode against the in-memory index. Vector
// recall returns nothing when q.Vector is empty; callers are expected
// to feed an embedded query vector for ModeVector / ModeHybrid.
//
// The function snapshots the per-dataset state under RLock, releases
// the lock, then scores outside the lock so long-running CPU work does
// not stall writers.
func (r *FSChunkRepo) Search(ctx context.Context, q knowledge.ChunkQuery) ([]knowledge.Candidate, error) {
	if q.TopK <= 0 {
		q.TopK = 5
	}
	mode := knowledge.ResolveMode(q.Mode)

	type snapshot struct {
		datasetID string
		state     *datasetState
	}
	var snaps []snapshot

	r.mu.RLock()
	if len(q.DatasetIDs) == 0 {
		for id, st := range r.states {
			snaps = append(snaps, snapshot{datasetID: id, state: st})
		}
	} else {
		for _, id := range q.DatasetIDs {
			if st, ok := r.states[id]; ok {
				snaps = append(snaps, snapshot{datasetID: id, state: st})
			}
		}
	}
	r.mu.RUnlock()

	tok := r.resolveTokenizer()
	var keywords []string
	if mode == knowledge.ModeBM25 || mode == knowledge.ModeHybrid {
		keywords = textsearch.ExtractKeywords(q.Text, tok)
	}

	var out []knowledge.Candidate
	for _, sn := range snaps {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if mode == knowledge.ModeBM25 || mode == knowledge.ModeHybrid {
			out = append(out, scoreBM25(sn.datasetID, sn.state, keywords)...)
		}
		if mode == knowledge.ModeVector || mode == knowledge.ModeHybrid {
			if len(q.Vector) > 0 {
				out = append(out, scoreVector(sn.datasetID, sn.state, q.Vector)...)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Hit.Score > out[j].Hit.Score })
	if q.TopK > 0 && len(out) > q.TopK*2 {
		out = out[:q.TopK*2]
	}
	return out, nil
}

// addToInverted updates the inverted index with one chunk's tokens.
func addToInverted(inv map[string][]chunkPosting, chunkIdx int, tokens []string) {
	if len(tokens) == 0 {
		return
	}
	tf := make(map[string]int, len(tokens))
	for _, t := range tokens {
		tf[t]++
	}
	for term, freq := range tf {
		inv[term] = append(inv[term], chunkPosting{chunkIdx: chunkIdx, tf: freq, dl: len(tokens)})
	}
}

// scoreBM25 produces BM25 candidates for the dataset against keywords.
// Source is "bm25" so the Ranker can fuse them with vector candidates.
func scoreBM25(datasetID string, state *datasetState, keywords []string) []knowledge.Candidate {
	if state == nil || len(keywords) == 0 || len(state.inverted) == 0 {
		return nil
	}
	const (
		k1 = 1.2
		b  = 0.75
	)
	avgDL := state.stats.AvgLength
	if avgDL <= 0 {
		avgDL = 1
	}
	scores := make(map[int]float64)
	for _, kw := range keywords {
		postings, ok := state.inverted[kw]
		if !ok {
			continue
		}
		df := state.stats.DocFreq[kw]
		n := float64(state.stats.DocCount)
		idf := math.Log((n-float64(df)+0.5)/(float64(df)+0.5) + 1.0)
		for _, p := range postings {
			dl := float64(p.dl)
			tfNorm := float64(p.tf) * (k1 + 1) / (float64(p.tf) + k1*(1-b+b*dl/avgDL))
			scores[p.chunkIdx] += idf * tfNorm
		}
	}
	out := make([]knowledge.Candidate, 0, len(scores))
	for idx, sc := range scores {
		c := state.chunks[idx]
		out = append(out, knowledge.Candidate{
			Source: "bm25",
			Hit: knowledge.Hit{
				DatasetID:  datasetID,
				DocName:    c.DocName,
				Layer:      knowledge.LayerDetail,
				Content:    c.Content,
				Score:      sc,
				ChunkIndex: c.Index,
				Sig:        c.Sig,
			},
		})
	}
	return out
}

// scoreVector produces cosine-similarity candidates for the dataset.
func scoreVector(datasetID string, state *datasetState, qvec []float32) []knowledge.Candidate {
	if state == nil || len(qvec) == 0 {
		return nil
	}
	out := make([]knowledge.Candidate, 0)
	for i := range state.chunks {
		c := state.chunks[i]
		if len(c.Vector) == 0 {
			continue
		}
		sim := knowledge.CosineSimilarity(qvec, c.Vector)
		if sim <= 0 {
			continue
		}
		out = append(out, knowledge.Candidate{
			Source: "vector",
			Hit: knowledge.Hit{
				DatasetID:  datasetID,
				DocName:    c.DocName,
				Layer:      knowledge.LayerDetail,
				Content:    c.Content,
				Score:      sim,
				ChunkIndex: c.Index,
				Sig:        c.Sig,
			},
		})
	}
	return out
}

// Compile-time interface assertion.
var _ knowledge.ChunkRepo = (*FSChunkRepo)(nil)
