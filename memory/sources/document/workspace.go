package document

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const documentSchemaVersion = 2

// Option configures a WorkspaceStore.
type Option func(*WorkspaceStore)

// WithClock replaces the clock used for authoritative timestamps.
func WithClock(clock func() time.Time) Option {
	return func(store *WorkspaceStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// WorkspaceStore persists each document in its own versioned JSON file. Calls
// through one store are safe for concurrent use. All writers in one process
// that share a workspace must share the same WorkspaceStore; the workspace API
// has no cross-instance or cross-process compare-and-swap.
type WorkspaceStore struct {
	ws    workspace.Workspace
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

type currentPointer struct {
	SchemaVersion int       `json:"schema_version"`
	RuntimeID     string    `json:"runtime_id"`
	UserID        string    `json:"user_id"`
	AgentID       string    `json:"agent_id,omitempty"`
	DatasetID     string    `json:"dataset_id"`
	DocumentID    string    `json:"document_id"`
	EventID       string    `json:"event_id"`
	EventKey      string    `json:"event_key"`
	Version       uint64    `json:"version"`
	Operation     Operation `json:"operation"`
}

type outboxHead struct {
	SchemaVersion int             `json:"schema_version"`
	Scope         sdkmemory.Scope `json:"scope"`
	LastSeq       uint64          `json:"last_seq"`
}

type pendingEvent struct {
	SchemaVersion  int             `json:"schema_version"`
	Scope          sdkmemory.Scope `json:"scope"`
	DatasetID      string          `json:"dataset_id"`
	DocumentID     string          `json:"document_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	OutboxSeq      uint64          `json:"outbox_seq"`
}

type eventLocator struct {
	SchemaVersion  int    `json:"schema_version"`
	DatasetID      string `json:"dataset_id"`
	DocumentID     string `json:"document_id"`
	IdempotencyKey string `json:"idempotency_key"`
	OutboxSeq      uint64 `json:"outbox_seq"`
}

var _ Store = (*WorkspaceStore)(nil)

// NewWorkspaceStore constructs a canonical document store.
func NewWorkspaceStore(ws workspace.Workspace, options ...Option) (*WorkspaceStore, error) {
	if nilWorkspace(ws) {
		return nil, errors.New("document source: workspace is required")
	}
	store := &WorkspaceStore{ws: ws, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Put publishes one immutable revision event and atomically advances current.
func (store *WorkspaceStore) Put(ctx context.Context, request PutRequest) (Document, error) {
	if err := validatePut(request); err != nil {
		return Document{}, err
	}
	content := request.Content.Clone()
	provenance := append([]sdkmemory.SourceRef(nil), request.Provenance...)
	metadata := request.Metadata.Clone()

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := store.prepareOutbox(ctx, request.Scope); err != nil {
		return Document{}, err
	}
	prior, ok, err := store.readEvent(ctx, request.Scope, request.DatasetID, request.DocumentID, request.IdempotencyKey)
	if err != nil {
		return Document{}, err
	}
	if ok {
		if prior.Event.Operation != OperationPut || prior.Event.Document == nil {
			return Document{}, errors.New("document source: idempotency key conflicts with non-put event")
		}
		if err := store.publishEvent(ctx, prior); err != nil {
			return Document{}, err
		}
		return cloneDocument(*prior.Event.Document), nil
	}

	current, hasCurrent, err := store.readCurrentEvent(ctx, request.Scope, request.DatasetID, request.DocumentID)
	if err != nil {
		return Document{}, err
	}
	head, _, err := store.readOutboxHead(ctx, request.Scope)
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
			events, err := store.scanDocumentEvents(ctx, request.Scope, request.DatasetID, request.DocumentID)
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
		DocumentID: request.DocumentID, Version: document.Version, OutboxSeq: head.LastSeq + 1, Document: &document,
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
	if err := store.writePendingEvent(ctx, pendingEvent{
		SchemaVersion: documentSchemaVersion, Scope: request.Scope,
		DatasetID: request.DatasetID, DocumentID: request.DocumentID,
		IdempotencyKey: request.IdempotencyKey, OutboxSeq: event.OutboxSeq,
	}); err != nil {
		return Document{}, err
	}
	if err := store.writeEvent(ctx, persisted); err != nil {
		return Document{}, err
	}
	if err := store.publishEvent(ctx, persisted); err != nil {
		return Document{}, err
	}
	return cloneDocument(document), nil
}

// Get returns one document within a hard partition and dataset.
func (store *WorkspaceStore) Get(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) (Document, bool, error) {
	if err := validateAddress(scope, datasetID); err != nil {
		return Document{}, false, err
	}
	if strings.TrimSpace(documentID) == "" {
		return Document{}, false, errors.New("document source: document_id is required")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	persisted, ok, err := store.readCurrentEvent(ctx, scope, datasetID, documentID)
	if err != nil || !ok {
		return Document{}, ok, err
	}
	if persisted.Event.Operation == OperationTombstone {
		return Document{}, false, nil
	}
	return cloneDocument(*persisted.Event.Document), true, nil
}

// List scans document files and returns values ordered by DocumentID.
// AfterID is exclusive.
func (store *WorkspaceStore) List(ctx context.Context, scope sdkmemory.Scope, datasetID string, options ListOptions) ([]Document, error) {
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
func (store *WorkspaceStore) ListDatasets(ctx context.Context, scope sdkmemory.Scope) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	entries, err := store.ws.List(ctx, store.datasetsDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("document source: list datasets: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, dataName, err := decodeSegment(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("document source: decode dataset path %q: %w", entry.Name(), err)
		}
		if !dataName {
			continue
		}
		documents, err := store.scanDataset(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		if len(documents) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ListEvents returns immutable revisions in scope-wide publication order.
func (store *WorkspaceStore) ListEvents(ctx context.Context, scope sdkmemory.Scope, options ListEventOptions) ([]Event, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.prepareOutbox(ctx, scope); err != nil {
		return nil, err
	}
	head, ok, err := store.readOutboxHead(ctx, scope)
	if err != nil || !ok || options.AfterOutboxSeq >= head.LastSeq {
		return []Event{}, err
	}
	events := make([]Event, 0)
	for seq := options.AfterOutboxSeq + 1; seq <= head.LastSeq; seq++ {
		locator, err := store.readEventLocator(ctx, scope, seq)
		if err != nil {
			return nil, err
		}
		persisted, ok, err := store.readEvent(
			ctx, scope, locator.DatasetID, locator.DocumentID, locator.IdempotencyKey,
		)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("document source: outbox event %d is missing", seq)
		}
		event := cloneEvent(persisted.Event)
		event.OutboxSeq = seq
		events = append(events, event)
		if options.Limit > 0 && len(events) == options.Limit {
			break
		}
	}
	return events, nil
}

// ListDocumentEvents returns one document's immutable revisions in version
// order without scanning or decoding sibling documents.
func (store *WorkspaceStore) ListDocumentEvents(
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
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.prepareOutbox(ctx, scope); err != nil {
		return nil, err
	}
	current, ok, err := store.readCurrentEvent(ctx, scope, datasetID, documentID)
	if err != nil || !ok || options.AfterVersion >= current.Event.Version {
		return []Event{}, err
	}
	events := make([]Event, 0)
	for version := options.AfterVersion + 1; version <= current.Event.Version; version++ {
		locator, err := store.readDocumentEventLocator(ctx, scope, datasetID, documentID, version)
		if err != nil {
			return nil, err
		}
		persisted, ok, err := store.readEvent(ctx, scope, datasetID, documentID, locator.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if !ok || persisted.Event.Version != version {
			return nil, fmt.Errorf("document source: document event version %d is missing", version)
		}
		event := cloneEvent(persisted.Event)
		event.OutboxSeq = locator.OutboxSeq
		events = append(events, event)
		if options.Limit > 0 && len(events) == options.Limit {
			break
		}
	}
	return events, nil
}

func (store *WorkspaceStore) scanAllEvents(ctx context.Context, scope sdkmemory.Scope) ([]persistedEvent, error) {
	datasets, err := store.ws.List(ctx, store.datasetsDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []persistedEvent{}, nil
		}
		return nil, fmt.Errorf("document source: list event datasets: %w", err)
	}
	var events []persistedEvent
	for _, datasetEntry := range datasets {
		if !datasetEntry.IsDir() {
			continue
		}
		datasetID, dataName, err := decodeSegment(datasetEntry.Name())
		if err != nil {
			return nil, fmt.Errorf("document source: decode event dataset path %q: %w", datasetEntry.Name(), err)
		}
		if !dataName {
			continue
		}
		documents, err := store.ws.List(ctx, store.documentsDir(scope, datasetID))
		if err != nil {
			return nil, fmt.Errorf("document source: list event documents: %w", err)
		}
		for _, documentEntry := range documents {
			if !documentEntry.IsDir() {
				if strings.HasPrefix(documentEntry.Name(), "k_") {
					return nil, fmt.Errorf("document source: data-like document path %q is not a directory", documentEntry.Name())
				}
				continue
			}
			documentID, dataName, err := decodeSegment(documentEntry.Name())
			if err != nil {
				return nil, fmt.Errorf("document source: decode event document path %q: %w", documentEntry.Name(), err)
			}
			if !dataName {
				continue
			}
			persisted, err := store.scanDocumentEvents(ctx, scope, datasetID, documentID)
			if err != nil {
				return nil, err
			}
			if err := store.ensureCurrentLatest(ctx, scope, datasetID, documentID, persisted); err != nil {
				return nil, err
			}
			for _, value := range persisted {
				events = append(events, value)
			}
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Event.DatasetID != events[j].Event.DatasetID {
			return events[i].Event.DatasetID < events[j].Event.DatasetID
		}
		if events[i].Event.DocumentID != events[j].Event.DocumentID {
			return events[i].Event.DocumentID < events[j].Event.DocumentID
		}
		return events[i].Event.Version < events[j].Event.Version
	})
	return events, nil
}

func (store *WorkspaceStore) prepareOutbox(ctx context.Context, scope sdkmemory.Scope) error {
	pending, hasPending, err := store.readPendingEvent(ctx, scope)
	if err != nil {
		return err
	}
	if hasPending {
		persisted, ok, err := store.readEvent(
			ctx, scope, pending.DatasetID, pending.DocumentID, pending.IdempotencyKey,
		)
		if err != nil {
			return err
		}
		if ok {
			if persisted.Event.OutboxSeq == 0 {
				persisted.Event.OutboxSeq = pending.OutboxSeq
			}
			if err := store.publishEvent(ctx, persisted); err != nil {
				return err
			}
		} else if err := store.deleteIfExists(ctx, store.outboxPendingPath(scope)); err != nil {
			return err
		}
	}
	if _, ok, err := store.readOutboxHead(ctx, scope); err != nil || ok {
		return err
	}
	events, err := store.scanAllEvents(ctx, scope)
	if err != nil {
		return err
	}
	for index, persisted := range events {
		persisted.Event.OutboxSeq = uint64(index + 1)
		if err := store.publishEvent(ctx, persisted); err != nil {
			return fmt.Errorf("document source: migrate legacy event %q: %w", persisted.Event.ID, err)
		}
	}
	return nil
}

func (store *WorkspaceStore) publishEvent(ctx context.Context, persisted persistedEvent) error {
	event := persisted.Event
	if event.OutboxSeq == 0 {
		seq, ok, err := store.findOutboxSequence(ctx, event.Scope, persisted)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("document source: outbox sequence is required for publication")
		}
		event.OutboxSeq = seq
		persisted.Event.OutboxSeq = seq
	}
	current, ok, err := store.readCurrentEvent(ctx, event.Scope, event.DatasetID, event.DocumentID)
	if err != nil {
		return err
	}
	if !ok || current.Event.Version <= event.Version {
		if err := store.writeCurrent(ctx, persisted); err != nil {
			return err
		}
	}
	locator := eventLocator{
		SchemaVersion: documentSchemaVersion, DatasetID: event.DatasetID,
		DocumentID: event.DocumentID, IdempotencyKey: persisted.IdempotencyKey,
		OutboxSeq: event.OutboxSeq,
	}
	if err := store.writeImmutableJSON(
		ctx,
		store.documentEventLocatorPath(event.Scope, event.DatasetID, event.DocumentID, event.Version),
		locator,
	); err != nil {
		return fmt.Errorf("document source: publish document event version %d: %w", event.Version, err)
	}
	if err := store.writeImmutableJSON(ctx, store.outboxEventPath(event.Scope, event.OutboxSeq), locator); err != nil {
		return fmt.Errorf("document source: publish outbox event %d: %w", event.OutboxSeq, err)
	}
	head, ok, err := store.readOutboxHead(ctx, event.Scope)
	if err != nil {
		return err
	}
	if ok && head.LastSeq > event.OutboxSeq {
		return store.deleteIfExists(ctx, store.outboxPendingPath(event.Scope))
	}
	if ok && head.LastSeq+1 < event.OutboxSeq {
		return fmt.Errorf("document source: outbox gap before sequence %d", event.OutboxSeq)
	}
	head = outboxHead{
		SchemaVersion: documentSchemaVersion, Scope: event.Scope, LastSeq: event.OutboxSeq,
	}
	if err := store.writeJSON(ctx, store.outboxHeadPath(event.Scope), head); err != nil {
		return fmt.Errorf("document source: publish outbox head: %w", err)
	}
	return store.deleteIfExists(ctx, store.outboxPendingPath(event.Scope))
}

func (store *WorkspaceStore) findOutboxSequence(
	ctx context.Context,
	scope sdkmemory.Scope,
	persisted persistedEvent,
) (uint64, bool, error) {
	entries, err := store.ws.List(ctx, path.Join(store.outboxDir(scope), "events"))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		seq, err := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("document source: invalid outbox locator %q", entry.Name())
		}
		locator, err := store.readEventLocator(ctx, scope, seq)
		if err != nil {
			return 0, false, err
		}
		if locator.DatasetID == persisted.DatasetID && locator.DocumentID == persisted.DocumentID &&
			locator.IdempotencyKey == persisted.IdempotencyKey {
			return seq, true, nil
		}
	}
	return 0, false, nil
}

func (store *WorkspaceStore) readOutboxHead(ctx context.Context, scope sdkmemory.Scope) (outboxHead, bool, error) {
	var head outboxHead
	ok, err := store.readJSON(ctx, store.outboxHeadPath(scope), &head)
	if err != nil || !ok {
		return outboxHead{}, ok, err
	}
	if head.SchemaVersion != documentSchemaVersion || head.Scope != scope || head.LastSeq == 0 {
		return outboxHead{}, false, errors.New("document source: corrupt outbox head")
	}
	return head, true, nil
}

func (store *WorkspaceStore) readPendingEvent(ctx context.Context, scope sdkmemory.Scope) (pendingEvent, bool, error) {
	var pending pendingEvent
	ok, err := store.readJSON(ctx, store.outboxPendingPath(scope), &pending)
	if err != nil || !ok {
		return pendingEvent{}, ok, err
	}
	if pending.SchemaVersion != documentSchemaVersion || pending.Scope != scope ||
		pending.DatasetID == "" || pending.DocumentID == "" ||
		pending.IdempotencyKey == "" || pending.OutboxSeq == 0 {
		return pendingEvent{}, false, errors.New("document source: corrupt pending event")
	}
	return pending, true, nil
}

func (store *WorkspaceStore) writePendingEvent(ctx context.Context, pending pendingEvent) error {
	if err := store.writeJSON(ctx, store.outboxPendingPath(pending.Scope), pending); err != nil {
		return fmt.Errorf("document source: reserve outbox sequence: %w", err)
	}
	return nil
}

func (store *WorkspaceStore) readEventLocator(ctx context.Context, scope sdkmemory.Scope, seq uint64) (eventLocator, error) {
	var locator eventLocator
	ok, err := store.readJSON(ctx, store.outboxEventPath(scope, seq), &locator)
	if err != nil {
		return eventLocator{}, err
	}
	if !ok || locator.SchemaVersion != documentSchemaVersion || locator.DatasetID == "" ||
		locator.DocumentID == "" || locator.IdempotencyKey == "" || locator.OutboxSeq != seq {
		return eventLocator{}, fmt.Errorf("document source: corrupt outbox event %d", seq)
	}
	return locator, nil
}

func (store *WorkspaceStore) readDocumentEventLocator(
	ctx context.Context,
	scope sdkmemory.Scope,
	datasetID string,
	documentID string,
	version uint64,
) (eventLocator, error) {
	var locator eventLocator
	ok, err := store.readJSON(
		ctx, store.documentEventLocatorPath(scope, datasetID, documentID, version), &locator,
	)
	if err != nil {
		return eventLocator{}, err
	}
	if !ok || locator.SchemaVersion != documentSchemaVersion ||
		locator.DatasetID != datasetID || locator.DocumentID != documentID ||
		locator.IdempotencyKey == "" || locator.OutboxSeq == 0 {
		return eventLocator{}, fmt.Errorf("document source: corrupt document event version %d", version)
	}
	return locator, nil
}

// Delete publishes a tombstone. Missing and already tombstoned documents are
// treated as success.
func (store *WorkspaceStore) Delete(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) error {
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
func (store *WorkspaceStore) DeleteDataset(ctx context.Context, scope sdkmemory.Scope, datasetID string) error {
	if err := validateAddress(scope, datasetID); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := store.ws.List(ctx, store.documentsDir(scope, datasetID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			if strings.HasPrefix(entry.Name(), "k_") {
				return fmt.Errorf("document source: data-like document path %q is not a directory", entry.Name())
			}
			continue
		}
		documentID, dataName, err := decodeSegment(entry.Name())
		if err != nil {
			return fmt.Errorf("document source: decode document path %q: %w", entry.Name(), err)
		}
		if !dataName {
			continue
		}
		if err := store.tombstone(ctx, scope, datasetID, documentID); err != nil {
			return err
		}
	}
	return nil
}

func (store *WorkspaceStore) scanDataset(ctx context.Context, scope sdkmemory.Scope, datasetID string) ([]Document, error) {
	entries, err := store.ws.List(ctx, store.documentsDir(scope, datasetID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []Document{}, nil
		}
		return nil, fmt.Errorf("document source: list dataset %q: %w", datasetID, err)
	}
	documents := make([]Document, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			if strings.HasPrefix(entry.Name(), "k_") {
				return nil, fmt.Errorf("document source: data-like document path %q is not a directory", entry.Name())
			}
			continue
		}
		documentID, dataName, err := decodeSegment(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("document source: decode document path %q: %w", entry.Name(), err)
		}
		if !dataName {
			continue
		}
		persisted, ok, err := store.readCurrentEvent(ctx, scope, datasetID, documentID)
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

func (store *WorkspaceStore) readCurrentEvent(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) (persistedEvent, bool, error) {
	data, err := store.ws.Read(ctx, store.currentPath(scope, datasetID, documentID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return persistedEvent{}, false, nil
		}
		return persistedEvent{}, false, fmt.Errorf("document source: read current %q: %w", documentID, err)
	}
	var current currentPointer
	if err := decodeJSON(data, &current); err != nil {
		return persistedEvent{}, false, fmt.Errorf("document source: decode current %q: %w", documentID, err)
	}
	if current.SchemaVersion != documentSchemaVersion || current.RuntimeID != scope.RuntimeID ||
		current.UserID != scope.UserID || current.AgentID != scope.AgentID ||
		current.DatasetID != datasetID || current.DocumentID != documentID ||
		current.EventID == "" || current.EventKey == "" || current.Version == 0 {
		return persistedEvent{}, false, fmt.Errorf("document source: corrupt current %q address or authority fields", documentID)
	}
	persisted, ok, err := store.readEvent(ctx, scope, datasetID, documentID, current.EventKey)
	if err != nil {
		return persistedEvent{}, false, err
	}
	if !ok {
		return persistedEvent{}, false, fmt.Errorf("document source: current event %q is missing", current.EventID)
	}
	if persisted.Event.ID != current.EventID || persisted.Event.Version != current.Version ||
		persisted.Event.Operation != current.Operation {
		return persistedEvent{}, false, errors.New("document source: current pointer does not match event")
	}
	return persisted, true, nil
}

func (store *WorkspaceStore) readEvent(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID, key string) (persistedEvent, bool, error) {
	data, err := store.ws.Read(ctx, store.eventPath(scope, datasetID, documentID, key))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return persistedEvent{}, false, nil
		}
		return persistedEvent{}, false, fmt.Errorf("document source: read event %q: %w", key, err)
	}
	var persisted persistedEvent
	if err := decodeJSON(data, &persisted); err != nil {
		return persistedEvent{}, false, fmt.Errorf("document source: decode event %q: %w", key, err)
	}
	if err := validatePersistedEvent(persisted, scope, datasetID, documentID, key); err != nil {
		return persistedEvent{}, false, fmt.Errorf("document source: corrupt event %q: %w", key, err)
	}
	return persisted, true, nil
}

func (store *WorkspaceStore) scanDocumentEvents(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) ([]persistedEvent, error) {
	entries, err := store.ws.List(ctx, store.eventsDir(scope, datasetID, documentID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []persistedEvent{}, nil
		}
		return nil, fmt.Errorf("document source: list events for %q: %w", documentID, err)
	}
	events := make([]persistedEvent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("document source: unexpected event directory %q", entry.Name())
		}
		key, dataName, err := decodeDataFilename(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("document source: decode event filename %q: %w", entry.Name(), err)
		}
		if !dataName {
			continue
		}
		event, ok, err := store.readEvent(ctx, scope, datasetID, documentID, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("document source: event %q disappeared during scan", key)
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Event.Version < events[j].Event.Version })
	for index, event := range events {
		if event.Event.Version != uint64(index+1) {
			return nil, fmt.Errorf("document source: non-contiguous event version %d, want %d", event.Event.Version, index+1)
		}
	}
	return events, nil
}

func validatePersistedEvent(persisted persistedEvent, scope sdkmemory.Scope, datasetID, documentID, key string) error {
	if persisted.SchemaVersion != documentSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", persisted.SchemaVersion)
	}
	if persisted.RuntimeID != scope.RuntimeID || persisted.UserID != scope.UserID ||
		persisted.AgentID != scope.AgentID || persisted.DatasetID != datasetID ||
		persisted.DocumentID != documentID || persisted.IdempotencyKey != key {
		return errors.New("persisted address does not match workspace path")
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
		return errors.New("document address does not match workspace path")
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

func (store *WorkspaceStore) writeEvent(ctx context.Context, persisted persistedEvent) error {
	if err := validatePersistedEvent(
		persisted, persisted.Event.Scope, persisted.DatasetID,
		persisted.DocumentID, persisted.IdempotencyKey,
	); err != nil {
		return fmt.Errorf("document source: invalid event %q: %w", persisted.Event.ID, err)
	}
	target := store.eventPath(persisted.Event.Scope, persisted.DatasetID, persisted.DocumentID, persisted.IdempotencyKey)
	existing, err := store.ws.Read(ctx, target)
	if err == nil {
		var prior persistedEvent
		if err := decodeJSON(existing, &prior); err != nil {
			return fmt.Errorf("document source: decode immutable event %q: %w", persisted.Event.ID, err)
		}
		if !reflect.DeepEqual(prior, persisted) {
			return errdefs.Conflictf("document source: immutable event %q conflicts", persisted.Event.ID)
		}
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("document source: inspect event %q: %w", persisted.Event.ID, err)
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("document source: encode event %q: %w", persisted.Event.ID, err)
	}
	if err := workspace.AtomicWrite(ctx, store.ws, target, data); err != nil {
		return fmt.Errorf("document source: write event %q: %w", persisted.Event.ID, err)
	}
	return nil
}

func (store *WorkspaceStore) writeCurrent(ctx context.Context, persisted persistedEvent) error {
	event := persisted.Event
	data, err := json.Marshal(currentPointer{
		SchemaVersion: documentSchemaVersion, RuntimeID: event.Scope.RuntimeID,
		UserID: event.Scope.UserID, AgentID: event.Scope.AgentID,
		DatasetID: event.DatasetID, DocumentID: event.DocumentID,
		EventID: event.ID, EventKey: persisted.IdempotencyKey,
		Version: event.Version, Operation: event.Operation,
	})
	if err != nil {
		return fmt.Errorf("document source: encode current %q: %w", event.DocumentID, err)
	}
	if err := workspace.AtomicWrite(ctx, store.ws, store.currentPath(event.Scope, event.DatasetID, event.DocumentID), data); err != nil {
		return fmt.Errorf("document source: publish current %q: %w", event.DocumentID, err)
	}
	return nil
}

func (store *WorkspaceStore) readJSON(ctx context.Context, name string, destination any) (bool, error) {
	data, err := store.ws.Read(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := decodeJSON(data, destination); err != nil {
		return false, fmt.Errorf("decode %q: %w", name, err)
	}
	return true, nil
}

func (store *WorkspaceStore) writeJSON(ctx context.Context, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return workspace.AtomicWrite(ctx, store.ws, name, data)
}

func (store *WorkspaceStore) writeImmutableJSON(ctx context.Context, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	existing, err := store.ws.Read(ctx, name)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return errdefs.Conflictf("document source: immutable locator %q conflicts", name)
		}
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	return workspace.AtomicWrite(ctx, store.ws, name, data)
}

func (store *WorkspaceStore) deleteIfExists(ctx context.Context, name string) error {
	err := store.ws.Delete(ctx, name)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (store *WorkspaceStore) repairCurrent(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string, candidate persistedEvent) error {
	events, err := store.scanDocumentEvents(ctx, scope, datasetID, documentID)
	if err != nil {
		return err
	}
	if len(events) > 0 && events[len(events)-1].Event.ID == candidate.Event.ID {
		return store.writeCurrent(ctx, candidate)
	}
	return nil
}

func (store *WorkspaceStore) ensureCurrentLatest(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string, events []persistedEvent) error {
	if len(events) == 0 {
		return nil
	}
	latest := events[len(events)-1]
	current, ok, err := store.readCurrentEvent(ctx, scope, datasetID, documentID)
	if err != nil {
		return err
	}
	if !ok || current.Event.Version < latest.Event.Version {
		return store.writeCurrent(ctx, latest)
	}
	if current.Event.ID != latest.Event.ID {
		return errors.New("document source: current pointer is ahead of or conflicts with event log")
	}
	return nil
}

func (store *WorkspaceStore) tombstone(ctx context.Context, scope sdkmemory.Scope, datasetID, documentID string) error {
	if err := store.prepareOutbox(ctx, scope); err != nil {
		return err
	}
	events, err := store.scanDocumentEvents(ctx, scope, datasetID, documentID)
	if err != nil || len(events) == 0 {
		return err
	}
	if err := store.ensureCurrentLatest(ctx, scope, datasetID, documentID, events); err != nil {
		return err
	}
	current := events[len(events)-1]
	if current.Event.Operation == OperationTombstone {
		return nil
	}
	version := uint64(len(events)) + 1
	head, _, err := store.readOutboxHead(ctx, scope)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("tombstone-%020d", version)
	event := Event{
		ID:        eventID(scope, datasetID, documentID, version, OperationTombstone),
		Operation: OperationTombstone, Scope: scope, DatasetID: datasetID,
		DocumentID: documentID, Version: version, OutboxSeq: head.LastSeq + 1,
		Provenance: append([]sdkmemory.SourceRef(nil), current.Event.Provenance...),
		CreatedAt:  store.clock(),
	}
	persisted := persistedEvent{
		SchemaVersion: documentSchemaVersion, RuntimeID: scope.RuntimeID,
		UserID: scope.UserID, AgentID: scope.AgentID, DatasetID: datasetID,
		DocumentID: documentID, IdempotencyKey: key, Event: event,
	}
	if err := store.writePendingEvent(ctx, pendingEvent{
		SchemaVersion: documentSchemaVersion, Scope: scope, DatasetID: datasetID,
		DocumentID: documentID, IdempotencyKey: key, OutboxSeq: event.OutboxSeq,
	}); err != nil {
		return err
	}
	if err := store.writeEvent(ctx, persisted); err != nil {
		return err
	}
	return store.publishEvent(ctx, persisted)
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

func (store *WorkspaceStore) partitionDir(scope sdkmemory.Scope) string {
	return path.Join("sources", "document", "v2", "partitions", encodeSegment(scope.RuntimeID),
		encodeSegment(scope.UserID), encodeSegment(scope.AgentID))
}

func (store *WorkspaceStore) datasetsDir(scope sdkmemory.Scope) string {
	return path.Join(store.partitionDir(scope), "datasets")
}

func (store *WorkspaceStore) outboxDir(scope sdkmemory.Scope) string {
	return path.Join(store.partitionDir(scope), "outbox")
}

func (store *WorkspaceStore) outboxHeadPath(scope sdkmemory.Scope) string {
	return path.Join(store.outboxDir(scope), "head.json")
}

func (store *WorkspaceStore) outboxPendingPath(scope sdkmemory.Scope) string {
	return path.Join(store.outboxDir(scope), "pending.json")
}

func (store *WorkspaceStore) outboxEventPath(scope sdkmemory.Scope, seq uint64) string {
	return path.Join(store.outboxDir(scope), "events", fmt.Sprintf("%020d.json", seq))
}

func (store *WorkspaceStore) datasetDir(scope sdkmemory.Scope, datasetID string) string {
	return path.Join(store.datasetsDir(scope), encodeSegment(datasetID))
}

func (store *WorkspaceStore) documentsDir(scope sdkmemory.Scope, datasetID string) string {
	return path.Join(store.datasetDir(scope, datasetID), "documents")
}

func (store *WorkspaceStore) documentDir(scope sdkmemory.Scope, datasetID, documentID string) string {
	return path.Join(store.documentsDir(scope, datasetID), encodeSegment(documentID))
}

func (store *WorkspaceStore) currentPath(scope sdkmemory.Scope, datasetID, documentID string) string {
	return path.Join(store.documentDir(scope, datasetID, documentID), "current.json")
}

func (store *WorkspaceStore) documentPath(scope sdkmemory.Scope, datasetID, documentID string) string {
	return store.currentPath(scope, datasetID, documentID)
}

func (store *WorkspaceStore) eventsDir(scope sdkmemory.Scope, datasetID, documentID string) string {
	return path.Join(store.documentDir(scope, datasetID, documentID), "events")
}

func (store *WorkspaceStore) eventPath(scope sdkmemory.Scope, datasetID, documentID, key string) string {
	return path.Join(store.eventsDir(scope, datasetID, documentID), encodeSegment(key)+".json")
}

func (store *WorkspaceStore) documentEventLocatorPath(
	scope sdkmemory.Scope,
	datasetID string,
	documentID string,
	version uint64,
) string {
	return path.Join(
		store.documentDir(scope, datasetID, documentID),
		"event-index", fmt.Sprintf("%020d.json", version),
	)
}

func encodeSegment(value string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeSegment(segment string) (string, bool, error) {
	if !strings.HasPrefix(segment, "k_") {
		return "", false, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, "k_"))
	if err != nil {
		return "", true, err
	}
	if encodeSegment(string(data)) != segment {
		return "", true, errors.New("non-canonical path segment")
	}
	return string(data), true, nil
}

func decodeDataFilename(name string) (string, bool, error) {
	if !strings.HasSuffix(name, ".json") {
		return "", false, nil
	}
	return decodeSegment(strings.TrimSuffix(name, ".json"))
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

func nilWorkspace(ws workspace.Workspace) bool {
	if ws == nil {
		return true
	}
	value := reflect.ValueOf(ws)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
