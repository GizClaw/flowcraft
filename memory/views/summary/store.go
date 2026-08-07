package summary

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

const schemaVersion = 1

const summaryRecordEventType = "summary.record"

const (
	defaultActiveCompactionThreshold = 32
	defaultActiveCacheLimit          = 64
)

type Option func(*SummaryStore)

func WithClock(clock func() time.Time) Option {
	return func(store *SummaryStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

func WithActiveCompactionThreshold(threshold int) Option {
	return func(store *SummaryStore) {
		if threshold > 0 {
			store.activeCompactionThreshold = threshold
		}
	}
}

// SummaryStore persists immutable summary records as storage.Log appends plus
// KV snapshots, and the active record catalog (base + delta segments + head)
// entirely in a storage.Store.
type SummaryStore struct {
	log                       storage.Log
	kv                        storage.Store
	clock                     func() time.Time
	activeCompactionThreshold int
	activeCacheLimit          int
	activeCache               map[string]activeCatalogCache
	activeCacheOrder          []string
	mu                        sync.RWMutex
}

type persistedRecord struct {
	SchemaVersion  int    `json:"schema_version"`
	RuntimeID      string `json:"runtime_id"`
	UserID         string `json:"user_id"`
	AgentID        string `json:"agent_id,omitempty"`
	ConversationID string `json:"conversation_id"`
	RecordID       string `json:"record_id"`
	Record         Record `json:"record"`
}

// NewSummaryStore constructs a Log+KV backed summary view.
func NewSummaryStore(log storage.Log, kv storage.Store, options ...Option) (*SummaryStore, error) {
	if nilValue(log) || nilValue(kv) {
		return nil, errors.New("summary view: log and store are required")
	}
	if _, ok := kv.(storage.PutIfAbsentStore); !ok {
		return nil, errors.New("summary view: store must support immutable writes")
	}
	store := &SummaryStore{
		log: log, kv: kv, clock: time.Now,
		activeCompactionThreshold: defaultActiveCompactionThreshold,
		activeCacheLimit:          defaultActiveCacheLimit,
		activeCache:               make(map[string]activeCatalogCache),
	}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Add appends one immutable summary record.
func (store *SummaryStore) Add(ctx context.Context, request AddRequest) (Record, error) {
	if ctx == nil {
		return Record{}, errors.New("summary view: context is required")
	}
	now := store.clock()
	record := Record{
		ID: strings.TrimSpace(request.ID), Scope: request.Scope, ConversationID: strings.TrimSpace(request.ConversationID),
		Level: request.Level, Text: strings.TrimSpace(request.Text), Content: request.Content.Clone(),
		Topics: normalizeStrings(request.Topics), InputIDs: normalizeOrderedStrings(request.InputIDs),
		SourceRefs: normalizeSourceRefs(request.SourceRefs), CoverageRange: request.CoverageRange,
		SourceDigest: strings.TrimSpace(request.SourceDigest), TransformSignature: strings.TrimSpace(request.TransformSignature),
		GenerationID: strings.TrimSpace(request.GenerationID), CreatedAt: now,
	}
	if record.Text == "" {
		record.Text = strings.TrimSpace(record.Content.Text())
	}
	if record.ID == "" {
		record.ID = StableID(record.Scope, record.ConversationID, record.Level, record.InputIDs,
			record.SourceDigest, record.TransformSignature)
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	existing, found, err := store.read(ctx, record.Scope, record.ConversationID, record.ID)
	if err != nil {
		return Record{}, err
	}
	if found {
		if equivalentRecord(existing, record) {
			return existing.Clone(), nil
		}
		return Record{}, errdefs.Conflictf("summary view: record %q conflicts", record.ID)
	}
	payload, err := json.Marshal(persistedRecord{
		SchemaVersion: schemaVersion, RuntimeID: record.Scope.RuntimeID, UserID: record.Scope.UserID,
		AgentID: record.Scope.AgentID, ConversationID: record.ConversationID, RecordID: record.ID, Record: record,
	})
	if err != nil {
		return Record{}, fmt.Errorf("summary view: encode %q: %w", record.ID, err)
	}
	stream, err := store.recordsStream(record.Scope, record.ConversationID)
	if err != nil {
		return Record{}, err
	}
	if _, err := store.log.Append(ctx, stream, []storage.Event{{
		Stream:  stream,
		Type:    summaryRecordEventType,
		Payload: payload,
	}}, storage.AppendOptions{IdempotencyKey: record.ID}); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return Record{}, errdefs.Conflictf("summary view: record %q conflicts", record.ID)
		}
		return Record{}, fmt.Errorf("summary view: append %q: %w", record.ID, err)
	}
	key, err := store.recordPath(record.Scope, record.ConversationID, record.ID)
	if err != nil {
		return Record{}, err
	}
	if err := store.putImmutable(ctx, key, payload); err != nil {
		return Record{}, fmt.Errorf("summary view: write %q: %w", record.ID, err)
	}
	return record.Clone(), nil
}

// LoadActive loads the current active manifest.
func (store *SummaryStore) LoadActive(
	ctx context.Context,
	scope sdkmemory.Scope,
	conversationID string,
) (Manifest, bool, error) {
	if ctx == nil {
		return Manifest{}, false, errors.New("summary view: context is required")
	}
	if err := validateAddress(scope, conversationID); err != nil {
		return Manifest{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.loadActiveLocked(ctx, scope, conversationID)
}

// PublishActive publishes one active manifest.
func (store *SummaryStore) PublishActive(ctx context.Context, manifest Manifest) error {
	if ctx == nil {
		return errors.New("summary view: context is required")
	}
	manifest = manifest.Clone()
	manifest.ConversationID = strings.TrimSpace(manifest.ConversationID)
	manifest.GenerationID = strings.TrimSpace(manifest.GenerationID)
	manifest.FrontierDigest = strings.TrimSpace(manifest.FrontierDigest)
	manifest.RecordIDs = normalizeOrderedStrings(manifest.RecordIDs)
	if manifest.PublishedAt.IsZero() {
		manifest.PublishedAt = store.clock()
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.publishActiveLocked(ctx, manifest)
}

// ListActive returns the active records of one conversation.
func (store *SummaryStore) ListActive(
	ctx context.Context,
	scope sdkmemory.Scope,
	conversationID string,
	options ListOptions,
) ([]Record, error) {
	if ctx == nil {
		return nil, errors.New("summary view: context is required")
	}
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	if options.Level != nil {
		if err := options.Level.Validate(); err != nil {
			return nil, err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	manifest, found, err := store.loadActiveLocked(ctx, scope, conversationID)
	if err != nil || !found {
		return []Record{}, err
	}
	if options.GenerationID != "" && options.GenerationID != manifest.GenerationID {
		return []Record{}, nil
	}
	result := make([]Record, 0, len(manifest.RecordIDs))
	for _, id := range manifest.RecordIDs {
		record, ok, readErr := store.read(ctx, scope, conversationID, id)
		if readErr != nil {
			return nil, readErr
		}
		if !ok {
			return nil, fmt.Errorf("summary view: active record %q is missing", id)
		}
		if options.Level != nil && record.Level != *options.Level {
			continue
		}
		result = append(result, record)
	}
	return result, nil
}

// Get returns one immutable record by ID.
func (store *SummaryStore) Get(ctx context.Context, scope sdkmemory.Scope, conversationID, id string) (Record, bool, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return Record{}, false, err
	}
	if strings.TrimSpace(id) == "" {
		return Record{}, false, errors.New("summary view: id is required")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.read(ctx, scope, conversationID, id)
}

// List scans the immutable record catalog for explicit repair and GC tooling.
func (store *SummaryStore) List(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListOptions) ([]Record, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	if options.Level != nil {
		if err := options.Level.Validate(); err != nil {
			return nil, err
		}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.listLocked(ctx, scope, conversationID, options)
}

func (store *SummaryStore) listLocked(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListOptions) ([]Record, error) {
	prefix, err := store.recordsDir(scope, conversationID)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("summary view: list: %w", err)
	}
	result := make([]Record, 0, len(entries))
	for _, entry := range entries {
		id, ok, err := recordIDFromKey(prefix, entry.Key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		record, found, err := store.read(ctx, scope, conversationID, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("summary view: record %q disappeared during scan", id)
		}
		if options.GenerationID != "" && record.GenerationID != options.GenerationID {
			continue
		}
		if options.Level != nil && record.Level != *options.Level {
			continue
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Level != result[j].Level {
			return result[i].Level < result[j].Level
		}
		if result[i].CoverageRange.StartSeq != result[j].CoverageRange.StartSeq {
			return result[i].CoverageRange.StartSeq < result[j].CoverageRange.StartSeq
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (store *SummaryStore) read(ctx context.Context, scope sdkmemory.Scope, conversationID, id string) (Record, bool, error) {
	key, err := store.recordPath(scope, conversationID, id)
	if err != nil {
		return Record{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("summary view: read %q: %w", id, err)
	}
	var value persistedRecord
	if err := decodeStrict(data, &value); err != nil {
		return Record{}, false, fmt.Errorf("summary view: decode %q: %w", id, err)
	}
	if value.SchemaVersion != schemaVersion {
		return Record{}, false, fmt.Errorf("summary view: unsupported schema_version %d", value.SchemaVersion)
	}
	if value.RuntimeID != scope.RuntimeID || value.UserID != scope.UserID || value.AgentID != scope.AgentID ||
		value.ConversationID != conversationID || value.RecordID != id ||
		value.Record.Scope != scope || value.Record.ConversationID != conversationID || value.Record.ID != id {
		return Record{}, false, errors.New("summary view: persisted address does not match record key")
	}
	if err := value.Record.Validate(); err != nil {
		return Record{}, false, fmt.Errorf("summary view: corrupt record %q: %w", id, err)
	}
	return value.Record.Clone(), true, nil
}

func (store *SummaryStore) loadActiveLocked(
	ctx context.Context,
	scope sdkmemory.Scope,
	conversationID string,
) (Manifest, bool, error) {
	manifest, _, found, err := store.materializeActiveLocked(ctx, scope, conversationID)
	return manifest, found, err
}

func (store *SummaryStore) putImmutable(ctx context.Context, key string, data []byte) error {
	put, ok := store.kv.(storage.PutIfAbsentStore)
	if !ok {
		return errors.New("summary view: store must support immutable writes")
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
	if !bytes.Equal(existing, data) {
		return errdefs.Conflictf("summary view: immutable record conflicts at %q", key)
	}
	return nil
}

func equivalentRecord(left, right Record) bool {
	left.ID, right.ID = "", ""
	left.CreatedAt, right.CreatedAt = time.Time{}, time.Time{}
	left.GenerationID, right.GenerationID = "", ""
	return reflect.DeepEqual(left, right)
}

func validateAddress(scope sdkmemory.Scope, conversationID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("summary view: conversation_id is required")
	}
	return nil
}

func (store *SummaryStore) recordsDir(scope sdkmemory.Scope, conversationID string) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "views/summary/v1/" + partition + "/conversations/" +
		storage.EncodeSegment(conversationID) + "/records", nil
}

func (store *SummaryStore) recordsStream(scope sdkmemory.Scope, conversationID string) (string, error) {
	dir, err := store.recordsDir(scope, conversationID)
	if err != nil {
		return "", err
	}
	return dir + "/events", nil
}

func (store *SummaryStore) recordPath(scope sdkmemory.Scope, conversationID, id string) (string, error) {
	dir, err := store.recordsDir(scope, conversationID)
	if err != nil {
		return "", err
	}
	return dir + "/" + storage.EncodeSegment(id) + ".json", nil
}

func (store *SummaryStore) manifestPath(scope sdkmemory.Scope, conversationID string) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "views/summary/v1/" + partition + "/conversations/" +
		storage.EncodeSegment(conversationID) + "/active.json", nil
}

func recordIDFromKey(prefix, key string) (string, bool, error) {
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == key {
		return "", false, errors.New("summary view: record key outside prefix")
	}
	if !strings.HasSuffix(suffix, ".json") {
		return "", false, nil
	}
	id, err := storage.DecodeSegment(strings.TrimSuffix(suffix, ".json"))
	if err != nil {
		return "", true, err
	}
	return id, true, nil
}

func decodeStrict(data []byte, destination any) error {
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
	ref := reflect.ValueOf(value)
	switch ref.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return ref.IsNil()
	default:
		return false
	}
}
