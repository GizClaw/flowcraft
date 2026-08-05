package summary

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
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const schemaVersion = 1

const (
	defaultActiveCompactionThreshold = 32
	defaultActiveCacheLimit          = 64
)

type Store interface {
	Add(context.Context, AddRequest) (Record, error)
	Get(context.Context, sdkmemory.Scope, string, string) (Record, bool, error)
	// List scans the immutable record catalog for explicit repair and GC
	// tooling. Serving paths must use ListActive.
	List(context.Context, sdkmemory.Scope, string, ListOptions) ([]Record, error)
	LoadActive(context.Context, sdkmemory.Scope, string) (Manifest, bool, error)
	PublishActive(context.Context, Manifest) error
	ListActive(context.Context, sdkmemory.Scope, string, ListOptions) ([]Record, error)
}

type Option func(*WorkspaceStore)

func WithClock(clock func() time.Time) Option {
	return func(store *WorkspaceStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

func WithActiveCompactionThreshold(threshold int) Option {
	return func(store *WorkspaceStore) {
		if threshold > 0 {
			store.activeCompactionThreshold = threshold
		}
	}
}

type WorkspaceStore struct {
	ws                        workspace.Workspace
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

type persistedManifest struct {
	SchemaVersion int      `json:"schema_version"`
	Manifest      Manifest `json:"manifest"`
}

func NewWorkspaceStore(ws workspace.Workspace, options ...Option) (*WorkspaceStore, error) {
	if nilValue(ws) {
		return nil, errors.New("summary view: workspace is required")
	}
	store := &WorkspaceStore{
		ws: ws, clock: time.Now,
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

func (store *WorkspaceStore) Add(ctx context.Context, request AddRequest) (Record, error) {
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
	if err := workspace.AtomicWrite(ctx, store.ws, store.recordPath(record.Scope, record.ConversationID, record.ID), payload); err != nil {
		return Record{}, fmt.Errorf("summary view: write %q: %w", record.ID, err)
	}
	return record.Clone(), nil
}

func (store *WorkspaceStore) LoadActive(
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

func (store *WorkspaceStore) PublishActive(ctx context.Context, manifest Manifest) error {
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

func (store *WorkspaceStore) ListActive(
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

func (store *WorkspaceStore) Get(ctx context.Context, scope sdkmemory.Scope, conversationID, id string) (Record, bool, error) {
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

func (store *WorkspaceStore) List(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListOptions) ([]Record, error) {
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

func (store *WorkspaceStore) listLocked(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListOptions) ([]Record, error) {
	entries, err := store.ws.List(ctx, store.recordsDir(scope, conversationID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []Record{}, nil
		}
		return nil, fmt.Errorf("summary view: list: %w", err)
	}
	result := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("summary view: unexpected directory %q", entry.Name())
		}
		id, ok, err := decodeFilename(entry.Name())
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

func (store *WorkspaceStore) read(ctx context.Context, scope sdkmemory.Scope, conversationID, id string) (Record, bool, error) {
	data, err := store.ws.Read(ctx, store.recordPath(scope, conversationID, id))
	if err != nil {
		if errdefs.IsNotFound(err) {
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
		return Record{}, false, errors.New("summary view: persisted address does not match path")
	}
	if err := value.Record.Validate(); err != nil {
		return Record{}, false, fmt.Errorf("summary view: corrupt record %q: %w", id, err)
	}
	return value.Record.Clone(), true, nil
}

func (store *WorkspaceStore) loadActiveLocked(
	ctx context.Context,
	scope sdkmemory.Scope,
	conversationID string,
) (Manifest, bool, error) {
	manifest, _, found, err := store.materializeActiveLocked(ctx, scope, conversationID)
	return manifest, found, err
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

func (store *WorkspaceStore) recordsDir(scope sdkmemory.Scope, conversationID string) string {
	return path.Join("views", "summary", "v1", "partitions", encode(scope.RuntimeID), encode(scope.UserID),
		encode(scope.AgentID), "conversations", encode(conversationID), "records")
}

func (store *WorkspaceStore) recordPath(scope sdkmemory.Scope, conversationID, id string) string {
	return path.Join(store.recordsDir(scope, conversationID), encode(id)+".json")
}

func (store *WorkspaceStore) manifestPath(scope sdkmemory.Scope, conversationID string) string {
	return path.Join("views", "summary", "v1", "partitions", encode(scope.RuntimeID), encode(scope.UserID),
		encode(scope.AgentID), "conversations", encode(conversationID), "active.json")
}

func encode(value string) string { return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value)) }

func decodeFilename(name string) (string, bool, error) {
	if !strings.HasSuffix(name, ".json") {
		return "", false, nil
	}
	segment := strings.TrimSuffix(name, ".json")
	if !strings.HasPrefix(segment, "k_") {
		return "", false, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, "k_"))
	if err != nil || encode(string(data)) != segment {
		return "", true, errors.New("summary view: invalid record filename")
	}
	return string(data), true, nil
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
