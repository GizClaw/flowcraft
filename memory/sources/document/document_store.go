package document

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const documentSchemaVersion = 2

const documentEventType = "document.event"

// Option configures a DocumentStore.
type Option func(*DocumentStore)

// WithClock replaces the clock used for authoritative timestamps.
func WithClock(clock func() time.Time) Option {
	return func(store *DocumentStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// DocumentStore persists each document as a current-value snapshot in a
// storage.Store plus one immutable revision event per Put/Delete in a
// storage.Log. The Log seq doubles as the scope-wide outbox sequence, so
// derivation watermarks (ListEvents AfterOutboxSeq) keep working.
type DocumentStore struct {
	log   storage.Log
	kv    storage.Store
	clock func() time.Time
	mu    sync.RWMutex
}

type persistedEvent struct {
	SchemaVersion  int    `json:"schema_version"`
	RuntimeID      string `json:"runtime_id"`
	UserID         string `json:"user_id"`
	AgentID        string `json:"agent_id,omitempty"`
	DatasetID      string `json:"dataset_id"`
	DocumentID     string `json:"document_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Event          Event  `json:"event"`
}

// NewDocumentStore constructs a Log+KV backed canonical document store.
func NewDocumentStore(log storage.Log, kv storage.Store, options ...Option) (*DocumentStore, error) {
	if nilValue(log) || nilValue(kv) {
		return nil, errors.New("document source: log and store are required")
	}
	if _, ok := kv.(storage.PutIfAbsentStore); !ok {
		return nil, errors.New("document source: store must support immutable writes")
	}
	store := &DocumentStore{log: log, kv: kv, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Put publishes one immutable revision event and atomically advances current.
func (store *DocumentStore) Put(ctx context.Context, request PutRequest) (Document, error) {
	if err := validatePut(request); err != nil {
		return Document{}, err
	}
	content := request.Content.Clone()
	provenance := append([]sdkmemory.SourceRef(nil), request.Provenance...)
	metadata := request.Metadata.Clone()

	store.mu.Lock()
	defer store.mu.Unlock()

	if prior, found, err := store.readByKey(ctx, request.Scope, request.DatasetID, request.DocumentID, request.IdempotencyKey); err != nil {
		return Document{}, err
	} else if found {
		if prior.Event.Operation != OperationPut || prior.Event.Document == nil {
			return Document{}, errors.New("document source: idempotency key conflicts with non-put event")
		}
		if err := store.publishCurrent(ctx, prior); err != nil {
			return Document{}, err
		}
		return cloneDocument(*prior.Event.Document), nil
	}

	current, hasCurrent, err := store.readCurrent(ctx, request.Scope, request.DatasetID, request.DocumentID)
	if err != nil {
		return Document{}, err
	}
	now := store.clock()
	version := uint64(1)
	createdAt := now
	if hasCurrent {
		version = current.Event.Version + 1
		if current.Event.Document != nil {
			createdAt = current.Event.Document.CreatedAt
		} else {
			events, err := store.documentEvents(ctx, request.Scope, request.DatasetID, request.DocumentID)
			if err != nil {
				return Document{}, err
			}
			for _, prior := range events {
				if prior.Event.Document != nil {
					createdAt = prior.Event.Document.CreatedAt
					break
				}
			}
		}
	}
	document := Document{
		Scope:      request.Scope,
		DatasetID:  request.DatasetID,
		DocumentID: request.DocumentID,
		Content:    content,
		Provenance: provenance,
		Metadata:   metadata,
		Version:    version,
		CreatedAt:  createdAt,
		UpdatedAt:  now,
	}
	event := Event{
		ID:        eventID(request.Scope, request.DatasetID, request.DocumentID, document.Version, OperationPut),
		Operation: OperationPut, Scope: request.Scope, DatasetID: request.DatasetID,
		DocumentID: request.DocumentID, Version: document.Version, Document: &document,
		Provenance: append([]sdkmemory.SourceRef(nil), provenance...), CreatedAt: now,
	}
	persisted := persistedEvent{
		SchemaVersion:  documentSchemaVersion,
		RuntimeID:      request.Scope.RuntimeID,
		UserID:         request.Scope.UserID,
		AgentID:        request.Scope.AgentID,
		DatasetID:      request.DatasetID,
		DocumentID:     request.DocumentID,
		IdempotencyKey: request.IdempotencyKey,
		Event:          event,
	}
	authoritative, err := store.appendEvent(ctx, persisted)
	if err != nil {
		return Document{}, err
	}
	if err := store.writeByKey(ctx, authoritative); err != nil {
		return Document{}, err
	}
	if err := store.publishCurrent(ctx, authoritative); err != nil {
		return Document{}, err
	}
	return cloneDocument(*authoritative.Event.Document), nil
}

// Get returns one document within a hard partition and dataset.
func (store *DocumentStore) Get(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) (Document, bool, error) {
	if err := validateAddress(scope, datasetID); err != nil {
		return Document{}, false, err
	}
	if strings.TrimSpace(documentID) == "" {
		return Document{}, false, errors.New("document source: document_id is required")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	persisted, ok, err := store.readCurrent(ctx, scope, datasetID, documentID)
	if err != nil || !ok {
		return Document{}, ok, err
	}
	if persisted.Event.Operation == OperationTombstone {
		return Document{}, false, nil
	}
	return cloneDocument(*persisted.Event.Document), true, nil
}

// List scans current documents and returns values ordered by DocumentID.
func (store *DocumentStore) List(ctx context.Context, scope sdkmemory.Scope, datasetID string, options ListOptions) ([]Document, error) {
	if err := validateAddress(scope, datasetID); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	documents, err := store.scanDataset(ctx, scope, datasetID)
	if err != nil {
		return nil, err
	}
	result := make([]Document, 0)
	for _, document := range documents {
		if document.DocumentID <= options.AfterID {
			continue
		}
		result = append(result, cloneDocument(document))
		if options.Limit > 0 && len(result) == options.Limit {
			break
		}
	}
	return result, nil
}

// ListDatasets returns non-empty dataset IDs in lexical order.
func (store *DocumentStore) ListDatasets(ctx context.Context, scope sdkmemory.Scope) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	prefix, err := store.currentPrefix(scope)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	datasets := make(map[string]struct{}, 0)
	for _, entry := range entries {
		datasetID, err := decodeDatasetKey(prefix, entry.Key)
		if err != nil {
			return nil, err
		}
		datasets[datasetID] = struct{}{}
	}
	ids := make([]string, 0, len(datasets))
	for datasetID := range datasets {
		documents, err := store.scanDataset(ctx, scope, datasetID)
		if err != nil {
			return nil, err
		}
		if len(documents) > 0 {
			ids = append(ids, datasetID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ListEvents returns immutable revisions in scope-wide publication order.
func (store *DocumentStore) ListEvents(ctx context.Context, scope sdkmemory.Scope, options ListEventOptions) ([]Event, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	stream, err := store.eventsStream(scope)
	if err != nil {
		return nil, err
	}
	logEvents, err := store.log.Read(ctx, stream, options.AfterOutboxSeq, options.Limit)
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(logEvents))
	for _, logEvent := range logEvents {
		persisted, err := store.persistedFromEvent(logEvent)
		if err != nil {
			return nil, err
		}
		event := cloneEvent(persisted.Event)
		event.OutboxSeq = logEvent.Seq
		events = append(events, event)
	}
	return events, nil
}

// ListDocumentEvents returns one document's immutable revisions in version
// order.
func (store *DocumentStore) ListDocumentEvents(
	ctx context.Context,
	scope sdkmemory.Scope,
	datasetID string,
	documentID string,
	options ListDocumentEventOptions,
) ([]Event, error) {
	if err := validateAddress(scope, datasetID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(documentID) == "" {
		return nil, errors.New("document source: document_id is required")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	events, err := store.documentEvents(ctx, scope, datasetID, documentID)
	if err != nil {
		return nil, err
	}
	result := make([]Event, 0)
	for _, persisted := range events {
		if persisted.Event.Version <= options.AfterVersion {
			continue
		}
		result = append(result, cloneEvent(persisted.Event))
		if options.Limit > 0 && len(result) == options.Limit {
			break
		}
	}
	return result, nil
}

// Delete publishes a tombstone. Missing and already tombstoned documents are
// treated as success.
func (store *DocumentStore) Delete(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) error {
	if err := validateAddress(scope, datasetID); err != nil {
		return err
	}
	if strings.TrimSpace(documentID) == "" {
		return errors.New("document source: document_id is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.tombstone(ctx, scope, datasetID, documentID)
}

// DeleteDataset publishes a tombstone for every current document.
func (store *DocumentStore) DeleteDataset(ctx context.Context, scope sdkmemory.Scope, datasetID string) error {
	if err := validateAddress(scope, datasetID); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	prefix, err := store.currentDatasetPrefix(scope, datasetID)
	if err != nil {
		return err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		_, documentID, err := decodeCurrentKey(prefix, entry.Key)
		if err != nil {
			return fmt.Errorf("document source: decode document key %q: %w", entry.Key, err)
		}
		if err := store.tombstone(ctx, scope, datasetID, documentID); err != nil {
			return err
		}
	}
	return nil
}

func (store *DocumentStore) tombstone(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) error {
	current, ok, err := store.readCurrent(ctx, scope, datasetID, documentID)
	if err != nil || !ok || current.Event.Operation == OperationTombstone {
		return err
	}
	version := current.Event.Version + 1
	key := fmt.Sprintf("tombstone-%020d", version)
	event := Event{
		ID:        eventID(scope, datasetID, documentID, version, OperationTombstone),
		Operation: OperationTombstone, Scope: scope, DatasetID: datasetID,
		DocumentID: documentID, Version: version,
		Provenance: append([]sdkmemory.SourceRef(nil), current.Event.Provenance...),
		CreatedAt:  store.clock(),
	}
	persisted := persistedEvent{
		SchemaVersion: documentSchemaVersion, RuntimeID: scope.RuntimeID,
		UserID: scope.UserID, AgentID: scope.AgentID, DatasetID: datasetID,
		DocumentID: documentID, IdempotencyKey: key, Event: event,
	}
	authoritative, err := store.appendEvent(ctx, persisted)
	if err != nil {
		return err
	}
	if err := store.writeByKey(ctx, authoritative); err != nil {
		return err
	}
	return store.publishCurrent(ctx, authoritative)
}

func (store *DocumentStore) scanDataset(ctx context.Context, scope sdkmemory.Scope, datasetID string) ([]Document, error) {
	prefix, err := store.currentDatasetPrefix(scope, datasetID)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("document source: list dataset %q: %w", datasetID, err)
	}
	documents := make([]Document, 0, len(entries))
	for _, entry := range entries {
		_, documentID, err := decodeCurrentKey(prefix, entry.Key)
		if err != nil {
			return nil, fmt.Errorf("document source: decode document key %q: %w", entry.Key, err)
		}
		persisted, ok, err := store.readCurrent(ctx, scope, datasetID, documentID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("document source: document %q disappeared during scan", documentID)
		}
		if persisted.Event.Operation == OperationPut {
			documents = append(documents, cloneDocument(*persisted.Event.Document))
		}
	}
	sort.Slice(documents, func(i, j int) bool {
		return documents[i].DocumentID < documents[j].DocumentID
	})
	return documents, nil
}

func (store *DocumentStore) documentEvents(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) ([]persistedEvent, error) {
	stream, err := store.eventsStream(scope)
	if err != nil {
		return nil, err
	}
	logEvents, err := store.log.Read(ctx, stream, 0, 0)
	if err != nil {
		return nil, err
	}
	events := make([]persistedEvent, 0)
	for _, logEvent := range logEvents {
		persisted, err := store.persistedFromEvent(logEvent)
		if err != nil {
			return nil, err
		}
		if persisted.Event.DatasetID == datasetID && persisted.Event.DocumentID == documentID {
			persisted.Event.OutboxSeq = logEvent.Seq
			events = append(events, persisted)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Event.Version < events[j].Event.Version })
	for index, event := range events {
		if event.Event.Version != uint64(index+1) {
			return nil, fmt.Errorf("document source: non-contiguous event version %d, want %d", event.Event.Version, index+1)
		}
	}
	return events, nil
}

func (store *DocumentStore) appendEvent(ctx context.Context, persisted persistedEvent) (persistedEvent, error) {
	if err := validatePersistedEvent(
		persisted, persisted.Event.Scope, persisted.DatasetID,
		persisted.DocumentID, persisted.IdempotencyKey,
	); err != nil {
		return persistedEvent{}, fmt.Errorf("document source: invalid event %q: %w", persisted.Event.ID, err)
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return persistedEvent{}, fmt.Errorf("document source: encode event %q: %w", persisted.Event.ID, err)
	}
	stream, err := store.eventsStream(persisted.Event.Scope)
	if err != nil {
		return persistedEvent{}, err
	}
	if _, err := store.log.Append(ctx, stream, []storage.Event{{
		Stream:  stream,
		Type:    documentEventType,
		Payload: payload,
	}}, storage.AppendOptions{IdempotencyKey: persisted.Event.ID}); err == nil {
		return persisted, nil
	} else if !errors.Is(err, storage.ErrConflict) {
		return persistedEvent{}, fmt.Errorf("document source: write event %q: %w", persisted.Event.ID, err)
	}
	// The event ID already exists: recover the authoritative prior revision
	// (retry with a different clock or content must return the original).
	events, readErr := store.log.Read(ctx, stream, 0, 0)
	if readErr != nil {
		return persistedEvent{}, readErr
	}
	for _, logEvent := range events {
		var prior persistedEvent
		if err := decodeJSON(logEvent.Payload, &prior); err != nil {
			continue
		}
		if prior.Event.ID == persisted.Event.ID {
			if err := validatePersistedEvent(prior, prior.Event.Scope, prior.DatasetID, prior.DocumentID, prior.IdempotencyKey); err != nil {
				return persistedEvent{}, fmt.Errorf("document source: corrupt prior event %q: %w", persisted.Event.ID, err)
			}
			return prior, nil
		}
	}
	return persistedEvent{}, errdefs.Conflictf("document source: immutable event %q conflicts", persisted.Event.ID)
}

func (store *DocumentStore) writeByKey(ctx context.Context, persisted persistedEvent) error {
	key, err := store.byKeyKey(persisted.Event.Scope, persisted.DatasetID, persisted.DocumentID, persisted.IdempotencyKey)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	put, ok := store.kv.(storage.PutIfAbsentStore)
	if !ok {
		return errors.New("document source: store must support immutable writes")
	}
	written, err := put.PutIfAbsent(ctx, key, payload)
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
	if !bytes.Equal(existing, payload) {
		return errdefs.Conflictf("document source: immutable event %q conflicts", persisted.Event.ID)
	}
	return nil
}

func (store *DocumentStore) publishCurrent(ctx context.Context, persisted persistedEvent) error {
	key, err := store.currentKey(persisted.Event.Scope, persisted.DatasetID, persisted.DocumentID)
	if err != nil {
		return err
	}
	if current, ok, err := store.readCurrent(ctx, persisted.Event.Scope, persisted.DatasetID, persisted.DocumentID); err != nil {
		return err
	} else if ok && current.Event.Version > persisted.Event.Version {
		return nil
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("document source: encode current %q: %w", persisted.Event.DocumentID, err)
	}
	if err := store.kv.Put(ctx, key, payload); err != nil {
		return fmt.Errorf("document source: publish current %q: %w", persisted.Event.DocumentID, err)
	}
	return nil
}

func (store *DocumentStore) readByKey(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID, key string) (persistedEvent, bool, error) {
	keyName, err := store.byKeyKey(scope, datasetID, documentID, key)
	if err != nil {
		return persistedEvent{}, false, err
	}
	data, err := store.kv.Get(ctx, keyName)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return persistedEvent{}, false, nil
		}
		return persistedEvent{}, false, err
	}
	var persisted persistedEvent
	if err := decodeJSON(data, &persisted); err != nil {
		return persistedEvent{}, false, err
	}
	if err := validatePersistedEvent(persisted, scope, datasetID, documentID, key); err != nil {
		return persistedEvent{}, false, err
	}
	return persisted, true, nil
}

func (store *DocumentStore) readCurrent(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) (persistedEvent, bool, error) {
	key, err := store.currentKey(scope, datasetID, documentID)
	if err != nil {
		return persistedEvent{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return persistedEvent{}, false, nil
		}
		return persistedEvent{}, false, fmt.Errorf("document source: read current %q: %w", documentID, err)
	}
	var persisted persistedEvent
	if err := decodeJSON(data, &persisted); err != nil {
		return persistedEvent{}, false, fmt.Errorf("document source: decode current %q: %w", documentID, err)
	}
	if err := validatePersistedEvent(persisted, scope, datasetID, documentID, persisted.IdempotencyKey); err != nil {
		return persistedEvent{}, false, fmt.Errorf("document source: corrupt current %q: %w", documentID, err)
	}
	return persisted, true, nil
}

func (store *DocumentStore) persistedFromEvent(logEvent storage.Event) (persistedEvent, error) {
	var persisted persistedEvent
	if err := decodeJSON(logEvent.Payload, &persisted); err != nil {
		return persistedEvent{}, fmt.Errorf("document source: decode event %d: %w", logEvent.Seq, err)
	}
	if err := validatePersistedEvent(persisted, persisted.Event.Scope, persisted.DatasetID, persisted.DocumentID, persisted.IdempotencyKey); err != nil {
		return persistedEvent{}, fmt.Errorf("document source: corrupt event %d: %w", logEvent.Seq, err)
	}
	return persisted, nil
}

func (store *DocumentStore) scopePrefix(scope sdkmemory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "sources/v1/document/" + partition, nil
}

func (store *DocumentStore) eventsStream(scope sdkmemory.Scope) (string, error) {
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/events", nil
}

func (store *DocumentStore) currentPrefix(scope sdkmemory.Scope) (string, error) {
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/current", nil
}

func (store *DocumentStore) currentDatasetPrefix(scope sdkmemory.Scope, datasetID string) (string, error) {
	prefix, err := store.currentPrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/" + storage.EncodeSegment(datasetID), nil
}

func (store *DocumentStore) currentKey(scope sdkmemory.Scope, datasetID, documentID string) (string, error) {
	prefix, err := store.currentDatasetPrefix(scope, datasetID)
	if err != nil {
		return "", err
	}
	return prefix + "/" + storage.EncodeSegment(documentID), nil
}

func (store *DocumentStore) byKeyKey(scope sdkmemory.Scope, datasetID, documentID, key string) (string, error) {
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/by-key/" + storage.EncodeSegment(datasetID) + "/" +
		storage.EncodeSegment(documentID) + "/" + storage.EncodeSegment(key), nil
}

func decodeCurrentKey(prefix, key string) (string, string, error) {
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == key {
		return "", "", errors.New("document key outside prefix")
	}
	segments := strings.Split(suffix, "/")
	if len(segments) != 1 {
		return "", "", errors.New("document key has unexpected depth")
	}
	documentID, err := storage.DecodeSegment(segments[0])
	if err != nil {
		return "", "", err
	}
	prefixSegments := strings.Split(prefix, "/")
	if len(prefixSegments) < 2 {
		return "", "", errors.New("invalid document prefix")
	}
	datasetID, err := storage.DecodeSegment(prefixSegments[len(prefixSegments)-1])
	if err != nil {
		return "", "", err
	}
	return datasetID, documentID, nil
}

func decodeDatasetKey(prefix, key string) (string, error) {
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == key {
		return "", errors.New("document key outside prefix")
	}
	segments := strings.Split(suffix, "/")
	if len(segments) != 2 {
		return "", errors.New("document key has unexpected depth")
	}
	return storage.DecodeSegment(segments[0])
}

func validatePersistedEvent(persisted persistedEvent, scope sdkmemory.Scope, datasetID, documentID, key string) error {
	if persisted.SchemaVersion != documentSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", persisted.SchemaVersion)
	}
	if persisted.RuntimeID != scope.RuntimeID || persisted.UserID != scope.UserID ||
		persisted.AgentID != scope.AgentID || persisted.DatasetID != datasetID ||
		persisted.DocumentID != documentID || persisted.IdempotencyKey != key {
		return errors.New("persisted address does not match document key")
	}
	event := persisted.Event
	if event.Scope != scope || event.DatasetID != datasetID || event.DocumentID != documentID ||
		event.Version == 0 || event.CreatedAt.IsZero() ||
		event.ID != eventID(scope, datasetID, documentID, event.Version, event.Operation) {
		return errors.New("event address or authority fields are invalid")
	}
	if len(event.Provenance) == 0 {
		return errors.New("event provenance is required")
	}
	for index, source := range event.Provenance {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("event provenance %d: %w", index, err)
		}
	}
	switch event.Operation {
	case OperationPut:
		if event.Document == nil {
			return errors.New("put event document is required")
		}
		if event.Document.Version != event.Version ||
			!reflect.DeepEqual(event.Document.Provenance, event.Provenance) {
			return errors.New("put event payload does not match event authority")
		}
		return validateDocument(*event.Document, scope, datasetID, documentID)
	case OperationTombstone:
		if event.Document != nil {
			return errors.New("tombstone event must not contain a document")
		}
		return nil
	default:
		return fmt.Errorf("unsupported operation %q", event.Operation)
	}
}

func validateDocument(document Document, scope sdkmemory.Scope, datasetID, documentID string) error {
	if document.Scope != scope ||
		document.DatasetID != datasetID || document.DocumentID != documentID {
		return errors.New("document address does not match document key")
	}
	if document.Version == 0 || document.CreatedAt.IsZero() || document.UpdatedAt.IsZero() {
		return errors.New("document has invalid authority fields")
	}
	if err := document.Content.Validate(); err != nil {
		return fmt.Errorf("content: %w", err)
	}
	if len(document.Provenance) == 0 {
		return errors.New("provenance is required")
	}
	for index, source := range document.Provenance {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("provenance %d: %w", index, err)
		}
	}
	return nil
}

func validatePut(request PutRequest) error {
	if err := validateAddress(request.Scope, request.DatasetID); err != nil {
		return err
	}
	if strings.TrimSpace(request.DocumentID) == "" {
		return errors.New("document source: document_id is required")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return errors.New("document source: idempotency_key is required")
	}
	if err := request.Content.Validate(); err != nil {
		return fmt.Errorf("document source: content: %w", err)
	}
	if len(request.Provenance) == 0 {
		return errors.New("document source: provenance is required")
	}
	for index, source := range request.Provenance {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("document source: provenance %d: %w", index, err)
		}
	}
	return nil
}

func validateAddress(scope sdkmemory.Scope, datasetID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(datasetID) == "" {
		return errors.New("document source: dataset_id is required")
	}
	return nil
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
