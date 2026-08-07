package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const schemaVersion = 1

// DocumentViewStore writes immutable chunk builds and atomically publishes a
// small pointer on a storage.Store. Calls through one instance are safe for
// concurrent use.
type DocumentViewStore struct {
	kv storage.Store
	mu sync.RWMutex
}

type activeBuild struct {
	SchemaVersion   int    `json:"schema_version"`
	RuntimeID       string `json:"runtime_id"`
	UserID          string `json:"user_id"`
	AgentID         string `json:"agent_id,omitempty"`
	DatasetID       string `json:"dataset_id"`
	DocumentID      string `json:"document_id"`
	DocumentVersion uint64 `json:"document_version"`
	BuildID         string `json:"build_id"`
	ChunkCount      int    `json:"chunk_count"`
}

type persistedChunk struct {
	SchemaVersion int    `json:"schema_version"`
	RuntimeID     string `json:"runtime_id"`
	UserID        string `json:"user_id"`
	AgentID       string `json:"agent_id,omitempty"`
	DatasetID     string `json:"dataset_id"`
	DocumentID    string `json:"document_id"`
	BuildID       string `json:"build_id"`
	ChunkID       string `json:"chunk_id"`
	Chunk         Chunk  `json:"chunk"`
}

// NewDocumentViewStore constructs a KV-backed document chunk view.
func NewDocumentViewStore(kv storage.Store) (*DocumentViewStore, error) {
	if nilValue(kv) {
		return nil, errors.New("document view: store is required")
	}
	if _, ok := kv.(storage.PutIfAbsentStore); !ok {
		return nil, errors.New("document view: store must support immutable writes")
	}
	return &DocumentViewStore{kv: kv}, nil
}

// ReplaceDocument publishes one immutable chunk build and moves the active
// pointer.
func (store *DocumentViewStore) ReplaceDocument(ctx context.Context, request ReplaceRequest) ([]Chunk, error) {
	chunks := cloneChunks(request.Chunks)
	for index := range chunks {
		normalizeRecord(&chunks[index])
	}
	request.Chunks = chunks
	if err := validateReplace(request); err != nil {
		return nil, err
	}
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].Ordinal == chunks[j].Ordinal {
			return chunks[i].ID < chunks[j].ID
		}
		return chunks[i].Ordinal < chunks[j].Ordinal
	})
	buildID, err := buildDigest(request, chunks)
	if err != nil {
		return nil, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists, err := store.readActive(ctx, request.Scope, request.DatasetID, request.DocumentID)
	if err != nil {
		return nil, err
	}
	if exists {
		if current.DocumentVersion > request.DocumentVersion {
			return []Chunk{}, nil
		}
		if current.DocumentVersion == request.DocumentVersion && current.BuildID == buildID {
			return cloneChunks(chunks), nil
		}
	}
	for _, chunk := range chunks {
		persisted := persistedChunk{
			SchemaVersion: schemaVersion, RuntimeID: request.Scope.RuntimeID,
			UserID: request.Scope.UserID, AgentID: request.Scope.AgentID, DatasetID: request.DatasetID,
			DocumentID: request.DocumentID, BuildID: buildID, ChunkID: chunk.ID,
			Chunk: chunk,
		}
		if err := store.writeImmutableChunk(ctx, persisted); err != nil {
			return nil, err
		}
	}
	active := activeBuild{
		SchemaVersion: schemaVersion, RuntimeID: request.Scope.RuntimeID,
		UserID: request.Scope.UserID, AgentID: request.Scope.AgentID, DatasetID: request.DatasetID,
		DocumentID: request.DocumentID, DocumentVersion: request.DocumentVersion,
		BuildID: buildID, ChunkCount: len(chunks),
	}
	data, err := json.Marshal(active)
	if err != nil {
		return nil, fmt.Errorf("document view: encode active build: %w", err)
	}
	activeKey, err := store.activeKey(request.Scope, request.DatasetID, request.DocumentID)
	if err != nil {
		return nil, err
	}
	if err := store.kv.Put(ctx, activeKey, data); err != nil {
		return nil, fmt.Errorf("document view: publish active build: %w", err)
	}
	return cloneChunks(chunks), nil
}

// Get returns one chunk from the active build.
func (store *DocumentViewStore) Get(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID, chunkID string) (Chunk, bool, error) {
	if err := validateAddress(scope, datasetID, documentID); err != nil {
		return Chunk{}, false, err
	}
	if strings.TrimSpace(chunkID) == "" {
		return Chunk{}, false, errors.New("document view: chunk_id is required")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	active, ok, err := store.readActive(ctx, scope, datasetID, documentID)
	if err != nil || !ok {
		return Chunk{}, false, err
	}
	persisted, ok, err := store.readChunk(ctx, active, scope, datasetID, documentID, chunkID)
	if err != nil || !ok {
		return Chunk{}, ok, err
	}
	return cloneChunk(persisted.Chunk), true, nil
}

// List returns chunks from the active build in (Ordinal, ID) order.
func (store *DocumentViewStore) List(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string, options ListOptions) ([]Chunk, error) {
	if err := validateAddress(scope, datasetID, documentID); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	active, ok, err := store.readActive(ctx, scope, datasetID, documentID)
	if err != nil || !ok {
		if !ok && err == nil {
			return []Chunk{}, nil
		}
		return nil, err
	}
	prefix, err := store.chunksPrefix(scope, datasetID, documentID, active.BuildID)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("document view: list active build %q: %w", active.BuildID, err)
	}
	chunks := make([]Chunk, 0, len(entries))
	for _, entry := range entries {
		id, err := chunkIDFromKey(prefix, entry.Key)
		if err != nil {
			return nil, fmt.Errorf("document view: decode chunk key %q: %w", entry.Key, err)
		}
		var persisted persistedChunk
		if err := decodeJSON(entry.Value, &persisted); err != nil {
			return nil, fmt.Errorf("document view: decode chunk %q: %w", id, err)
		}
		normalizeRecord(&persisted.Chunk)
		if err := validatePersistedChunk(persisted, active, scope, datasetID, documentID, id); err != nil {
			return nil, fmt.Errorf("document view: corrupt chunk %q: %w", id, err)
		}
		chunks = append(chunks, cloneChunk(persisted.Chunk))
	}
	if len(chunks) != active.ChunkCount {
		return nil, fmt.Errorf("document view: active build chunk count %d, found %d", active.ChunkCount, len(chunks))
	}
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].Ordinal == chunks[j].Ordinal {
			return chunks[i].ID < chunks[j].ID
		}
		return chunks[i].Ordinal < chunks[j].Ordinal
	})
	result := make([]Chunk, 0)
	for _, chunk := range chunks {
		if chunk.Ordinal < options.AfterOrdinal ||
			(chunk.Ordinal == options.AfterOrdinal && chunk.ID <= options.AfterID) {
			continue
		}
		result = append(result, cloneChunk(chunk))
		if options.Limit > 0 && len(result) == options.Limit {
			break
		}
	}
	return result, nil
}

func (store *DocumentViewStore) writeImmutableChunk(ctx context.Context, persisted persistedChunk) error {
	key, err := store.chunkKey(persisted.Chunk.Scope, persisted.DatasetID, persisted.DocumentID, persisted.BuildID, persisted.ChunkID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("document view: encode chunk %q: %w", persisted.ChunkID, err)
	}
	put, ok := store.kv.(storage.PutIfAbsentStore)
	if !ok {
		return errors.New("document view: store must support immutable writes")
	}
	written, err := put.PutIfAbsent(ctx, key, data)
	if err != nil {
		return err
	}
	if written {
		return nil
	}
	existing, err := store.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	var prior persistedChunk
	if err := decodeJSON(existing, &prior); err != nil {
		return fmt.Errorf("document view: decode existing immutable chunk %q: %w", persisted.ChunkID, err)
	}
	if !reflect.DeepEqual(prior, persisted) {
		return errdefs.Conflictf("document view: immutable chunk %q conflicts", persisted.ChunkID)
	}
	return nil
}

func (store *DocumentViewStore) readActive(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) (activeBuild, bool, error) {
	key, err := store.activeKey(scope, datasetID, documentID)
	if err != nil {
		return activeBuild{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return activeBuild{}, false, nil
		}
		return activeBuild{}, false, fmt.Errorf("document view: read active build: %w", err)
	}
	var active activeBuild
	if err := decodeJSON(data, &active); err != nil {
		return activeBuild{}, false, fmt.Errorf("document view: decode active build: %w", err)
	}
	if active.SchemaVersion != schemaVersion {
		return activeBuild{}, false, fmt.Errorf("document view: unsupported active schema_version %d", active.SchemaVersion)
	}
	if active.RuntimeID != scope.RuntimeID || active.UserID != scope.UserID ||
		active.AgentID != scope.AgentID ||
		active.DatasetID != datasetID || active.DocumentID != documentID ||
		active.DocumentVersion == 0 || active.BuildID == "" || active.ChunkCount < 0 {
		return activeBuild{}, false, errors.New("document view: corrupt active build address or authority fields")
	}
	return active, true, nil
}

func (store *DocumentViewStore) readChunk(ctx context.Context, active activeBuild, scope sdkmemory.Scope, datasetID, documentID, chunkID string) (persistedChunk, bool, error) {
	key, err := store.chunkKey(scope, datasetID, documentID, active.BuildID, chunkID)
	if err != nil {
		return persistedChunk{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return persistedChunk{}, false, nil
		}
		return persistedChunk{}, false, fmt.Errorf("document view: read chunk %q: %w", chunkID, err)
	}
	var persisted persistedChunk
	if err := decodeJSON(data, &persisted); err != nil {
		return persistedChunk{}, false, fmt.Errorf("document view: decode chunk %q: %w", chunkID, err)
	}
	normalizeRecord(&persisted.Chunk)
	if err := validatePersistedChunk(persisted, active, scope, datasetID, documentID, chunkID); err != nil {
		return persistedChunk{}, false, fmt.Errorf("document view: corrupt chunk %q: %w", chunkID, err)
	}
	return persisted, true, nil
}

func (store *DocumentViewStore) documentPrefix(scope sdkmemory.Scope, datasetID, documentID string) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "views/v1/documents/" + partition + "/" +
		storage.EncodeSegment(datasetID) + "/" +
		storage.EncodeSegment(documentID), nil
}

func (store *DocumentViewStore) activeKey(scope sdkmemory.Scope, datasetID, documentID string) (string, error) {
	prefix, err := store.documentPrefix(scope, datasetID, documentID)
	if err != nil {
		return "", err
	}
	return prefix + "/active", nil
}

func (store *DocumentViewStore) chunksPrefix(scope sdkmemory.Scope, datasetID, documentID, buildID string) (string, error) {
	prefix, err := store.documentPrefix(scope, datasetID, documentID)
	if err != nil {
		return "", err
	}
	return prefix + "/builds/" + storage.EncodeSegment(buildID) + "/chunks", nil
}

func (store *DocumentViewStore) chunkKey(scope sdkmemory.Scope, datasetID, documentID, buildID, chunkID string) (string, error) {
	prefix, err := store.chunksPrefix(scope, datasetID, documentID, buildID)
	if err != nil {
		return "", err
	}
	return prefix + "/" + storage.EncodeSegment(chunkID), nil
}

func chunkIDFromKey(prefix, key string) (string, error) {
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == key {
		return "", errors.New("chunk key outside build prefix")
	}
	return storage.DecodeSegment(suffix)
}

func validateReplace(request ReplaceRequest) error {
	if err := validateAddress(request.Scope, request.DatasetID, request.DocumentID); err != nil {
		return err
	}
	if request.DocumentVersion == 0 {
		return errors.New("document view: document_version must be positive")
	}
	ids := make(map[string]struct{}, len(request.Chunks))
	for index, chunk := range request.Chunks {
		if err := validateChunk(chunk, request.Scope, request.DatasetID, request.DocumentID, request.DocumentVersion); err != nil {
			return fmt.Errorf("document view: chunk %d: %w", index, err)
		}
		if _, exists := ids[chunk.ID]; exists {
			return fmt.Errorf("document view: duplicate chunk id %q", chunk.ID)
		}
		ids[chunk.ID] = struct{}{}
	}
	for _, record := range request.Chunks {
		if record.ParentID == "" {
			continue
		}
		if _, exists := ids[record.ParentID]; !exists {
			return fmt.Errorf("document view: record %q has missing parent %q", record.ID, record.ParentID)
		}
	}
	return nil
}

func validateChunk(chunk Chunk, scope sdkmemory.Scope, datasetID, documentID string, version uint64) error {
	if strings.TrimSpace(chunk.ID) == "" {
		return errors.New("chunk id is required")
	}
	switch chunk.Kind {
	case KindResource, KindSection, KindChunk, KindSummary:
	default:
		return fmt.Errorf("unsupported record kind %q", chunk.Kind)
	}
	if chunk.Level < 0 {
		return errors.New("record level must not be negative")
	}
	if strings.TrimSpace(chunk.SourceDigest) == "" || strings.TrimSpace(chunk.TransformSignature) == "" {
		return errors.New("source_digest and transform_signature are required")
	}
	if chunk.Scope != scope || chunk.DatasetID != datasetID || chunk.DocumentID != documentID ||
		chunk.DocumentVersion != version {
		return errors.New("chunk address does not match replacement")
	}
	if err := chunk.Content.Validate(); err != nil {
		return fmt.Errorf("content: %w", err)
	}
	if len(chunk.Provenance) == 0 {
		return errors.New("provenance is required")
	}
	for index, source := range chunk.Provenance {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("provenance %d: %w", index, err)
		}
	}
	return nil
}

func normalizeRecord(record *Chunk) {
	if record.Kind == "" {
		record.Kind = KindChunk
		record.Level = 2
	}
	if record.SourceDigest == "" {
		data, _ := json.Marshal(record.Provenance)
		sum := sha256.Sum256(data)
		record.SourceDigest = hex.EncodeToString(sum[:])
	}
	if record.TransformSignature == "" {
		record.TransformSignature = "legacy"
	}
}

func validatePersistedChunk(value persistedChunk, active activeBuild, scope sdkmemory.Scope, datasetID, documentID, chunkID string) error {
	if value.SchemaVersion != schemaVersion {
		return fmt.Errorf("unsupported schema_version %d", value.SchemaVersion)
	}
	if value.RuntimeID != scope.RuntimeID || value.UserID != scope.UserID ||
		value.AgentID != scope.AgentID ||
		value.DatasetID != datasetID || value.DocumentID != documentID ||
		value.BuildID != active.BuildID || value.ChunkID != chunkID {
		return errors.New("persisted address does not match document key")
	}
	return validateChunk(value.Chunk, scope, datasetID, documentID, active.DocumentVersion)
}

func validateAddress(scope sdkmemory.Scope, datasetID, documentID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(datasetID) == "" {
		return errors.New("document view: dataset_id is required")
	}
	if strings.TrimSpace(documentID) == "" {
		return errors.New("document view: document_id is required")
	}
	return nil
}

func buildDigest(request ReplaceRequest, chunks []Chunk) (string, error) {
	payload := struct {
		Scope           sdkmemory.Scope `json:"scope"`
		DatasetID       string          `json:"dataset_id"`
		DocumentID      string          `json:"document_id"`
		DocumentVersion uint64          `json:"document_version"`
		Chunks          []Chunk         `json:"chunks"`
	}{request.Scope, request.DatasetID, request.DocumentID, request.DocumentVersion, chunks}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("document view: encode build identity: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func decodeJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
