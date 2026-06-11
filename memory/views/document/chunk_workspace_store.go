package document

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// ChunkWorkspaceStore persists document chunks as JSON files in a workspace.
//
// Each chunk is stored under its current layer for efficient layer scans:
// datasets/{encodedDatasetID}/documents/{encodedDocumentID}/layers/{encodedLayerIdentity}/chunks/{encodedChunkID}.json
//
// Chunk IDs are unique within a dataset/document. Re-putting the same chunk ID
// in a different layer replaces the older layer's copy so GetChunk remains
// unambiguous while Layer stays a filtering dimension.
//
// Concurrent writes to the same scoped workspace must go through one
// ChunkWorkspaceStore instance. Cross-instance or cross-process writers require
// an external lock or a workspace backend with stronger concurrency guarantees.
type ChunkWorkspaceStore struct {
	ws                workspace.Workspace
	pathSegmentPrefix string
	tmpCounter        atomic.Uint64

	mu sync.RWMutex
}

var _ ChunkStore = (*ChunkWorkspaceStore)(nil)

// defaultChunkPathSegmentPrefix marks encoded workspace path segments. It is
// not part of Chunk IDs, dataset IDs, document IDs, layer identities, or other
// business identifiers.
const defaultChunkPathSegmentPrefix = "chunk_"

// ChunkWorkspaceStoreOption configures a ChunkWorkspaceStore.
type ChunkWorkspaceStoreOption interface {
	applyChunkWorkspaceStore(*ChunkWorkspaceStore)
}

type chunkPathSegmentPrefixOption string

// WithChunkPathSegmentPrefix sets the encoded workspace path segment marker.
func WithChunkPathSegmentPrefix(prefix string) ChunkWorkspaceStoreOption {
	return chunkPathSegmentPrefixOption(prefix)
}

func (o chunkPathSegmentPrefixOption) applyChunkWorkspaceStore(s *ChunkWorkspaceStore) {
	s.pathSegmentPrefix = string(o)
}

// NewChunkWorkspaceStore returns a workspace-backed ChunkStore.
func NewChunkWorkspaceStore(ws workspace.Workspace, opts ...ChunkWorkspaceStoreOption) *ChunkWorkspaceStore {
	s := &ChunkWorkspaceStore{
		ws:                ws,
		pathSegmentPrefix: defaultChunkPathSegmentPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyChunkWorkspaceStore(s)
		}
	}
	return s
}

// PutChunk stores a chunk as the authoritative value for its dataset, document,
// layer, and chunk id.
func (s *ChunkWorkspaceStore) PutChunk(ctx context.Context, chunk Chunk) (Chunk, error) {
	if s.ws == nil {
		return Chunk{}, errdefs.Validationf("%s: workspace is required", chunksErrPrefix)
	}
	if err := validateChunk(chunk); err != nil {
		return Chunk{}, err
	}

	chunk = cloneChunk(chunk)
	now := time.Now().UTC()
	if chunk.CreatedAt.IsZero() {
		chunk.CreatedAt = now
	}
	if chunk.UpdatedAt.IsZero() {
		chunk.UpdatedAt = chunk.CreatedAt
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	datasetID := chunk.Scope.DatasetID
	livePath := s.chunkPath(datasetID, chunk.DocumentID, chunk.Layer, chunk.ID)
	stalePaths, err := s.staleChunkIDPathsLocked(ctx, datasetID, chunk.DocumentID, chunk.ID, livePath)
	if err != nil {
		return Chunk{}, err
	}
	if err := s.writeChunk(ctx, chunk); err != nil {
		return Chunk{}, err
	}
	if err := s.deleteStaleChunkIDPathsLocked(ctx, datasetID, chunk.DocumentID, chunk.ID, stalePaths); err != nil {
		return Chunk{}, err
	}
	return cloneChunk(chunk), nil
}

// GetChunk returns one chunk by scope, document, and chunk id.
func (s *ChunkWorkspaceStore) GetChunk(ctx context.Context, scope views.Scope, documentID string, id ChunkID) (Chunk, bool, error) {
	if s.ws == nil {
		return Chunk{}, false, errdefs.Validationf("%s: workspace is required", chunksErrPrefix)
	}
	if err := validateDatasetScope(scope); err != nil {
		return Chunk{}, false, err
	}
	if documentID == "" {
		return Chunk{}, false, errdefs.Validationf("%s: document_id is required", chunksErrPrefix)
	}
	if id == "" {
		return Chunk{}, false, errdefs.Validationf("%s: chunk id is required", chunksErrPrefix)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	datasetID := scope.DatasetID
	candidates, err := s.chunkCandidates(ctx, datasetID, documentID)
	if err != nil {
		return Chunk{}, false, err
	}
	for _, candidate := range candidates {
		if candidate.id != id {
			continue
		}
		chunk, ok, err := s.readChunkAtPath(ctx, candidate.path, datasetID, documentID, id)
		if err != nil {
			return Chunk{}, false, err
		}
		if !ok {
			continue
		}
		return cloneChunk(chunk), true, nil
	}
	return Chunk{}, false, nil
}

// ListChunks returns chunks ordered by ascending chunk id. When two layers have
// the same chunk id, the layer identity provides a deterministic tie-breaker.
func (s *ChunkWorkspaceStore) ListChunks(ctx context.Context, documentID string, opts ListOptions) ([]Chunk, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", chunksErrPrefix)
	}
	if opts.Scope == nil {
		return nil, errdefs.Validationf("%s: scope is required", chunksErrPrefix)
	}
	if err := validateDatasetScope(*opts.Scope); err != nil {
		return nil, err
	}
	if documentID == "" {
		return nil, errdefs.Validationf("%s: document_id is required", chunksErrPrefix)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	datasetID := opts.Scope.DatasetID
	candidates, err := s.chunkCandidates(ctx, datasetID, documentID)
	if err != nil {
		return nil, err
	}

	out := make([]Chunk, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.id <= opts.AfterID {
			continue
		}
		chunk, ok, err := s.readChunkAtPath(ctx, candidate.path, datasetID, documentID, candidate.id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if opts.Layer != nil && !sameLayer(chunk.Layer, *opts.Layer) {
			continue
		}
		out = append(out, cloneChunk(chunk))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

func (s *ChunkWorkspaceStore) staleChunkIDPathsLocked(ctx context.Context, datasetID, documentID string, id ChunkID, keepPath string) ([]string, error) {
	candidates, err := s.chunkCandidates(ctx, datasetID, documentID)
	if err != nil {
		return nil, err
	}
	stalePaths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.id != id || candidate.path == keepPath {
			continue
		}
		stalePaths = append(stalePaths, candidate.path)
	}
	return stalePaths, nil
}

func (s *ChunkWorkspaceStore) deleteStaleChunkIDPathsLocked(ctx context.Context, datasetID, documentID string, id ChunkID, stalePaths []string) error {
	for _, stalePath := range stalePaths {
		if err := s.ws.Delete(ctx, stalePath); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("%s: delete stale chunk %q/%q/%q: %w", chunksErrPrefix, datasetID, documentID, id, err)
		}
	}
	return nil
}

// DeleteDocument removes all persisted chunks for a document across layers. It
// is idempotent.
func (s *ChunkWorkspaceStore) DeleteDocument(ctx context.Context, scope views.Scope, documentID string) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", chunksErrPrefix)
	}
	if err := validateDatasetScope(scope); err != nil {
		return err
	}
	if documentID == "" {
		return errdefs.Validationf("%s: document_id is required", chunksErrPrefix)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	datasetID := scope.DatasetID
	if err := s.ws.RemoveAll(ctx, s.documentDir(datasetID, documentID)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete document %q/%q chunks: %w", chunksErrPrefix, datasetID, documentID, err)
	}
	return nil
}

// DeleteDataset removes all persisted chunks for a dataset. It is idempotent.
func (s *ChunkWorkspaceStore) DeleteDataset(ctx context.Context, scope views.Scope) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", chunksErrPrefix)
	}
	if err := validateDatasetScope(scope); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	datasetID := scope.DatasetID
	if err := s.ws.RemoveAll(ctx, s.datasetDir(datasetID)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete dataset %q chunks: %w", chunksErrPrefix, datasetID, err)
	}
	return nil
}

func validateDatasetScope(scope views.Scope) error {
	if err := scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid scope: %w", chunksErrPrefix, err)
	}
	if scope.DatasetID == "" {
		return errdefs.Validationf("%s: dataset_id is required", chunksErrPrefix)
	}
	return nil
}

func (s *ChunkWorkspaceStore) chunkCandidates(ctx context.Context, datasetID, documentID string) ([]chunkCandidate, error) {
	layerEntries, err := s.ws.List(ctx, s.layersDir(datasetID, documentID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list document %q/%q chunk layers: %w", chunksErrPrefix, datasetID, documentID, err)
	}

	candidates := make([]chunkCandidate, 0)
	for _, layerEntry := range layerEntries {
		if !layerEntry.IsDir() || !strings.HasPrefix(layerEntry.Name(), s.pathSegmentPrefix) {
			continue
		}
		layerIdentity, err := s.rawPathSegment(layerEntry.Name())
		if err != nil {
			return nil, fmt.Errorf("%s: decode chunk layer identity %q: %w", chunksErrPrefix, layerEntry.Name(), err)
		}
		chunksDir := path.Join(s.layersDir(datasetID, documentID), layerEntry.Name(), "chunks")
		chunkEntries, err := s.ws.List(ctx, chunksDir)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("%s: list document %q/%q chunks: %w", chunksErrPrefix, datasetID, documentID, err)
		}
		for _, chunkEntry := range chunkEntries {
			if chunkEntry.IsDir() || !strings.HasSuffix(chunkEntry.Name(), ".json") {
				continue
			}
			segment := strings.TrimSuffix(chunkEntry.Name(), ".json")
			if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
				continue
			}
			rawID, err := s.rawPathSegment(segment)
			if err != nil {
				return nil, fmt.Errorf("%s: decode chunk id %q: %w", chunksErrPrefix, chunkEntry.Name(), err)
			}
			candidates = append(candidates, chunkCandidate{
				id:            ChunkID(rawID),
				layerIdentity: layerIdentity,
				path:          path.Join(chunksDir, chunkEntry.Name()),
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].id != candidates[j].id {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].layerIdentity < candidates[j].layerIdentity
	})
	return candidates, nil
}

type chunkCandidate struct {
	id            ChunkID
	layerIdentity string
	path          string
}

func (s *ChunkWorkspaceStore) readChunkAtPath(ctx context.Context, chunkPath, datasetID, documentID string, id ChunkID) (Chunk, bool, error) {
	data, err := s.ws.Read(ctx, chunkPath)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Chunk{}, false, nil
		}
		return Chunk{}, false, fmt.Errorf("%s: read chunk %q/%q/%q: %w", chunksErrPrefix, datasetID, documentID, id, err)
	}

	var chunk Chunk
	if err := decodeChunk(data, &chunk); err != nil {
		return Chunk{}, false, fmt.Errorf("%s: decode chunk %q/%q/%q: %w", chunksErrPrefix, datasetID, documentID, id, err)
	}
	return chunk, true, nil
}

func (s *ChunkWorkspaceStore) writeChunk(ctx context.Context, chunk Chunk) error {
	data, err := encodeChunk(chunk)
	if err != nil {
		return fmt.Errorf("%s: marshal chunk %q/%q/%q: %w", chunksErrPrefix, chunk.Scope.DatasetID, chunk.DocumentID, chunk.ID, err)
	}

	livePath := s.chunkPath(chunk.Scope.DatasetID, chunk.DocumentID, chunk.Layer, chunk.ID)
	tmpPath := s.tmpChunkPath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write chunk tmp %q/%q/%q: %w", chunksErrPrefix, chunk.Scope.DatasetID, chunk.DocumentID, chunk.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish chunk %q/%q/%q: %w", chunksErrPrefix, chunk.Scope.DatasetID, chunk.DocumentID, chunk.ID, err)
	}
	return nil
}

func (s *ChunkWorkspaceStore) tmpChunkPath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *ChunkWorkspaceStore) datasetDir(datasetID string) string {
	return path.Join("datasets", s.pathSegment(datasetID))
}

func (s *ChunkWorkspaceStore) documentDir(datasetID, documentID string) string {
	return path.Join(s.datasetDir(datasetID), "documents", s.pathSegment(documentID))
}

func (s *ChunkWorkspaceStore) layersDir(datasetID, documentID string) string {
	return path.Join(s.documentDir(datasetID, documentID), "layers")
}

func (s *ChunkWorkspaceStore) layerDir(datasetID, documentID string, layer Layer) string {
	return path.Join(s.layersDir(datasetID, documentID), s.pathSegment(layerIdentity(layer)))
}

func (s *ChunkWorkspaceStore) chunksDir(datasetID, documentID string, layer Layer) string {
	return path.Join(s.layerDir(datasetID, documentID, layer), "chunks")
}

func (s *ChunkWorkspaceStore) chunkPath(datasetID, documentID string, layer Layer, id ChunkID) string {
	return path.Join(s.chunksDir(datasetID, documentID, layer), s.pathSegment(string(id))+".json")
}

func (s *ChunkWorkspaceStore) pathSegment(id string) string {
	return s.pathSegmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (s *ChunkWorkspaceStore) rawPathSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
		return "", fmt.Errorf("missing %q prefix", s.pathSegmentPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, s.pathSegmentPrefix))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func layerIdentity(layer Layer) string {
	data, err := json.Marshal(layerIdentityRecord(layer))
	if err != nil {
		panic(err)
	}
	return string(data)
}

type layerIdentityRecord struct {
	Name               string `json:"name"`
	Version            string `json:"version"`
	TransformSignature string `json:"transform_signature"`
}

type chunkRecord struct {
	ID         ChunkID             `json:"id"`
	Scope      views.Scope         `json:"scope"`
	DocumentID string              `json:"document_id"`
	Layer      Layer               `json:"layer"`
	Ordinal    int                 `json:"ordinal"`
	Span       views.Span          `json:"span"`
	Text       string              `json:"text"`
	SourceRef  views.SourceRef     `json:"source_ref"`
	Signature  views.ViewSignature `json:"signature"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
	Metadata   map[string]any      `json:"metadata,omitempty"`
}

func encodeChunk(chunk Chunk) ([]byte, error) {
	return json.Marshal(chunkRecord{
		ID:         chunk.ID,
		Scope:      chunk.Scope,
		DocumentID: chunk.DocumentID,
		Layer:      chunk.Layer,
		Ordinal:    chunk.Ordinal,
		Span:       chunk.Span,
		Text:       chunk.Text,
		SourceRef:  cloneSourceRef(chunk.SourceRef),
		Signature:  cloneViewSignature(chunk.Signature),
		CreatedAt:  chunk.CreatedAt,
		UpdatedAt:  chunk.UpdatedAt,
		Metadata:   cloneAnyMap(chunk.Metadata),
	})
}

func decodeChunk(data []byte, chunk *Chunk) error {
	var record chunkRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	*chunk = Chunk{
		ID:         record.ID,
		Scope:      record.Scope,
		DocumentID: record.DocumentID,
		Layer:      record.Layer,
		Ordinal:    record.Ordinal,
		Span:       record.Span,
		Text:       record.Text,
		SourceRef:  cloneSourceRef(record.SourceRef),
		Signature:  cloneViewSignature(record.Signature),
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
		Metadata:   cloneAnyMap(record.Metadata),
	}
	return nil
}
