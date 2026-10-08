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

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/errdefs"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const schemaVersion = 1

// DocumentViewStore writes immutable chunk builds and atomically publishes a
// small pointer on a storage.Store. Calls through one instance are safe for
// concurrent use.
//
// The view is not generation scoped: a chunk is addressed by the document
// provenance and text it was derived from, so an earlier policy reproduces the
// build it published rather than needing to be rolled back to. RetireStaleBuilds
// is what keeps the superseded builds from accumulating.
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
func (store *DocumentViewStore) Get(ctx context.Context, scope corememory.Scope, datasetID, documentID, chunkID string) (Chunk, bool, error) {
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
func (store *DocumentViewStore) List(ctx context.Context, scope corememory.Scope, datasetID, documentID string, options ListOptions) ([]Chunk, error) {
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

// ListBuilds returns every build stored for one document, sorted by build id.
// A build is written immutably and never overwritten, so a document that was
// published more than once -- a new revision, a re-derivation under changed
// chunking, a pointer that failed after its chunks were written -- has one
// build per chunk set it ever published, and readers resolve only the active
// one. The result is what RetireStaleBuilds sweeps.
func (store *DocumentViewStore) ListBuilds(
	ctx context.Context,
	scope corememory.Scope,
	datasetID, documentID string,
) ([]string, error) {
	if err := validateAddress(scope, datasetID, documentID); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, errors.New("document view: context is required")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.listBuildsLocked(ctx, scope, datasetID, documentID)
}

// RetireStaleBuilds drops the stored chunks of every build of one document
// except the active one, and reports how many chunk records it removed.
//
// This view is not generation scoped, and it does not need to be: a chunk id is
// derived from the document's provenance and text, not from the policy that
// derived it, so deriving a document again under an earlier policy reproduces
// the build it published then. There is therefore nothing to roll back to and
// nothing to keep for one: a superseded build has no reader, because Get and
// List resolve the active pointer, and its chunks are its only trace -- the
// projection lanes reconcile their entries themselves when a document changes
// (see the worker's knowledge delta). Retiring it is how the space comes back.
//
// A document with no active pointer keeps none of its builds: its chunks were
// written by a publish that did not finish, and the next ReplaceDocument writes
// whatever build it needs -- immutably, so a build retired here and published
// again is written again. Retiring is a maintenance action, like the rest of
// the sweep surface: run it while the documents of the scope are quiesced, or
// accept that a publish racing it leaves its own build in place.
func (store *DocumentViewStore) RetireStaleBuilds(
	ctx context.Context,
	scope corememory.Scope,
	datasetID, documentID string,
) (int, error) {
	if err := validateAddress(scope, datasetID, documentID); err != nil {
		return 0, err
	}
	if ctx == nil {
		return 0, errors.New("document view: context is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.retireStaleBuildsLocked(ctx, scope, datasetID, documentID)
}

// RetireScopeBuilds runs RetireStaleBuilds over every document of one scope,
// and reports how many chunk records it removed in total. It is the entry point
// a scope-wide maintenance pass uses, since the document view is addressed by
// dataset and document rather than by conversation.
func (store *DocumentViewStore) RetireScopeBuilds(ctx context.Context, scope corememory.Scope) (int, error) {
	if ctx == nil {
		return 0, errors.New("document view: context is required")
	}
	root, err := documentsRoot(scope)
	if err != nil {
		return 0, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := store.kv.List(ctx, root)
	if err != nil {
		return 0, fmt.Errorf("document view: list documents: %w", err)
	}
	addresses := make([]documentAddress, 0, len(entries))
	seen := make(map[documentAddress]struct{}, len(entries))
	for _, entry := range entries {
		address, err := documentAddressFromKey(root, entry.Key)
		if err != nil {
			return 0, fmt.Errorf("document view: decode document key %q: %w", entry.Key, err)
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool {
		if addresses[i].DatasetID == addresses[j].DatasetID {
			return addresses[i].DocumentID < addresses[j].DocumentID
		}
		return addresses[i].DatasetID < addresses[j].DatasetID
	})
	removed := 0
	for _, address := range addresses {
		count, err := store.retireStaleBuildsLocked(ctx, scope, address.DatasetID, address.DocumentID)
		if err != nil {
			return removed, err
		}
		removed += count
	}
	return removed, nil
}

type documentAddress struct {
	DatasetID  string
	DocumentID string
}

func (store *DocumentViewStore) retireStaleBuildsLocked(
	ctx context.Context,
	scope corememory.Scope,
	datasetID, documentID string,
) (int, error) {
	active, found, err := store.readActive(ctx, scope, datasetID, documentID)
	if err != nil {
		return 0, err
	}
	builds, err := store.listBuildsLocked(ctx, scope, datasetID, documentID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, build := range builds {
		if found && build == active.BuildID {
			continue
		}
		count, err := store.retireBuildLocked(ctx, scope, datasetID, documentID, build)
		if err != nil {
			return removed, err
		}
		removed += count
	}
	return removed, nil
}

func (store *DocumentViewStore) listBuildsLocked(
	ctx context.Context,
	scope corememory.Scope,
	datasetID, documentID string,
) ([]string, error) {
	prefix, err := store.buildsPrefix(scope, datasetID, documentID)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("document view: list builds: %w", err)
	}
	seen := make(map[string]struct{}, len(entries))
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		buildID, err := buildIDFromKey(prefix, entry.Key)
		if err != nil {
			return nil, fmt.Errorf("document view: decode build key %q: %w", entry.Key, err)
		}
		if _, ok := seen[buildID]; ok {
			continue
		}
		seen[buildID] = struct{}{}
		result = append(result, buildID)
	}
	sort.Strings(result)
	return result, nil
}

func (store *DocumentViewStore) retireBuildLocked(
	ctx context.Context,
	scope corememory.Scope,
	datasetID, documentID, buildID string,
) (int, error) {
	prefix, err := store.chunksPrefix(scope, datasetID, documentID, buildID)
	if err != nil {
		return 0, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("document view: list build %q: %w", buildID, err)
	}
	removed := 0
	for _, entry := range entries {
		if _, err := chunkIDFromKey(prefix, entry.Key); err != nil {
			return removed, fmt.Errorf("document view: build %q: %w", buildID, err)
		}
		if err := store.kv.Delete(ctx, entry.Key); err != nil {
			return removed, fmt.Errorf("document view: retire chunk %q: %w", entry.Key, err)
		}
		removed++
	}
	return removed, nil
}

func documentAddressFromKey(root, key string) (documentAddress, error) {
	suffix := strings.TrimPrefix(key, root+"/")
	if suffix == key {
		return documentAddress{}, errors.New("document key outside scope root")
	}
	parts := strings.Split(suffix, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return documentAddress{}, errors.New("document key has no dataset and document address")
	}
	datasetID, err := storage.DecodeSegment(parts[0])
	if err != nil {
		return documentAddress{}, err
	}
	documentID, err := storage.DecodeSegment(parts[1])
	if err != nil {
		return documentAddress{}, err
	}
	return documentAddress{DatasetID: datasetID, DocumentID: documentID}, nil
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

func (store *DocumentViewStore) readActive(ctx context.Context, scope corememory.Scope, datasetID, documentID string) (activeBuild, bool, error) {
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

func (store *DocumentViewStore) readChunk(ctx context.Context, active activeBuild, scope corememory.Scope, datasetID, documentID, chunkID string) (persistedChunk, bool, error) {
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

func (store *DocumentViewStore) documentPrefix(scope corememory.Scope, datasetID, documentID string) (string, error) {
	root, err := documentsRoot(scope)
	if err != nil {
		return "", err
	}
	return root + "/" +
		storage.EncodeSegment(datasetID) + "/" +
		storage.EncodeSegment(documentID), nil
}

func documentsRoot(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "views/v1/documents/" + partition, nil
}

func (store *DocumentViewStore) activeKey(scope corememory.Scope, datasetID, documentID string) (string, error) {
	prefix, err := store.documentPrefix(scope, datasetID, documentID)
	if err != nil {
		return "", err
	}
	return prefix + "/active", nil
}

func (store *DocumentViewStore) buildsPrefix(scope corememory.Scope, datasetID, documentID string) (string, error) {
	prefix, err := store.documentPrefix(scope, datasetID, documentID)
	if err != nil {
		return "", err
	}
	return prefix + "/builds", nil
}

func (store *DocumentViewStore) chunksPrefix(scope corememory.Scope, datasetID, documentID, buildID string) (string, error) {
	prefix, err := store.buildsPrefix(scope, datasetID, documentID)
	if err != nil {
		return "", err
	}
	return prefix + "/" + storage.EncodeSegment(buildID) + "/chunks", nil
}

func (store *DocumentViewStore) chunkKey(scope corememory.Scope, datasetID, documentID, buildID, chunkID string) (string, error) {
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

func buildIDFromKey(prefix, key string) (string, error) {
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == key {
		return "", errors.New("build key outside builds prefix")
	}
	segment, _, _ := strings.Cut(suffix, "/")
	if segment == "" {
		return "", errors.New("build key has no build id")
	}
	return storage.DecodeSegment(segment)
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
	// A parent cycle would make chunk hierarchy walks (retrieval parent
	// expansion) loop forever.
	byID := make(map[string]string, len(request.Chunks))
	for _, record := range request.Chunks {
		byID[record.ID] = record.ParentID
	}
	for _, record := range request.Chunks {
		seen := map[string]struct{}{record.ID: {}}
		for parent := record.ParentID; parent != ""; parent = byID[parent] {
			if _, exists := seen[parent]; exists {
				return fmt.Errorf("document view: record %q has a parent cycle", record.ID)
			}
			seen[parent] = struct{}{}
		}
	}
	return nil
}

func validateChunk(chunk Chunk, scope corememory.Scope, datasetID, documentID string, version uint64) error {
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

func validatePersistedChunk(value persistedChunk, active activeBuild, scope corememory.Scope, datasetID, documentID, chunkID string) error {
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

func validateAddress(scope corememory.Scope, datasetID, documentID string) error {
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
		Scope           corememory.Scope `json:"scope"`
		DatasetID       string           `json:"dataset_id"`
		DocumentID      string           `json:"document_id"`
		DocumentVersion uint64           `json:"document_version"`
		Chunks          []Chunk          `json:"chunks"`
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
