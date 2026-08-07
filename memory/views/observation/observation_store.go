package observation

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
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const observationEventType = "observation.event"

type envelope[T any] struct {
	SchemaVersion int `json:"schema_version"`
	Value         T   `json:"value"`
}

// ObservationStore persists immutable observation base snapshots in a
// storage.Store and lifecycle events as appends in a storage.Log.
type ObservationStore struct {
	log   storage.Log
	kv    storage.Store
	clock func() time.Time
	mu    sync.Mutex
}

// NewObservationStore constructs a Log+KV backed observation view.
func NewObservationStore(log storage.Log, kv storage.Store, options ...Option) (*ObservationStore, error) {
	if nilValue(log) || nilValue(kv) {
		return nil, errors.New("observation view: log and store are required")
	}
	if _, ok := kv.(storage.PutIfAbsentStore); !ok {
		return nil, errors.New("observation view: store must support immutable writes")
	}
	store := &ObservationStore{log: log, kv: kv, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Integrate appends immutable records. Replaying a task repairs a partial
// supersede sequence and returns the same observation.
func (store *ObservationStore) Integrate(ctx context.Context, value factview.Fact, taskID string) (Observation, error) {
	if ctx == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(value.ID) == "" {
		return Observation{}, errors.New("observation view: context, fact_id, and task_id are required")
	}
	if err := value.Scope.Validate(); err != nil {
		return Observation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.clock().UTC()
	if now.IsZero() {
		return Observation{}, errors.New("observation view: clock returned zero")
	}
	key := CanonicalKey(value)
	id := stableID("observation", taskID, value.ID)
	if existing, err := store.getBaseLocked(ctx, value.Scope, id); err == nil {
		if existing.FactID != value.ID || existing.Key != key || existing.Scope != value.Scope {
			return Observation{}, errdefs.Conflictf("observation view: stable identity conflicts")
		}
		if err := store.finishIntegration(ctx, existing, taskID); err != nil {
			return Observation{}, err
		}
		return store.getLocked(ctx, value.Scope, id)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Observation{}, err
	}
	current, found, err := store.currentLocked(ctx, value.Scope, key)
	if err != nil {
		return Observation{}, err
	}
	replaces := ""
	if found {
		if current.ID == id {
			replaces = current.Replaces
		} else {
			replaces = current.ID
		}
	}
	observation := Observation{
		ID: id, Scope: value.Scope, Key: key, FactID: value.ID, ConversationID: value.ConversationID,
		State: StateActive, Replaces: replaces, Provenance: append([]sdkmemory.SourceRef(nil), value.Provenance...),
		SourceDigest: value.SourceDigest, EventTime: value.EventTime, CreatedAt: now,
	}
	if observation.EventTime.IsZero() {
		observation.EventTime = value.CreatedAt
	}
	observationKey, err := store.observationKey(value.Scope, id)
	if err != nil {
		return Observation{}, err
	}
	if err := store.putImmutable(ctx, observationKey, observation); err != nil {
		return Observation{}, err
	}
	if err := store.finishIntegration(ctx, observation, taskID); err != nil {
		return Observation{}, err
	}
	return store.getLocked(ctx, value.Scope, id)
}

func (store *ObservationStore) finishIntegration(ctx context.Context, observation Observation, taskID string) error {
	if observation.Replaces != "" {
		if err := store.appendEvent(ctx, Event{
			ID: stableID("supersede", taskID, observation.Replaces, observation.ID), Scope: observation.Scope, Kind: EventSuperseded,
			ObservationID: observation.Replaces, RelatedID: observation.ID, TaskID: taskID, Time: observation.CreatedAt,
		}); err != nil {
			return err
		}
	}
	if err := store.appendEvent(ctx, Event{
		ID: stableID("integrate", taskID, observation.ID), Scope: observation.Scope, Kind: EventIntegrated,
		ObservationID: observation.ID, RelatedID: observation.Replaces, TaskID: taskID, Time: observation.CreatedAt,
	}); err != nil {
		return err
	}
	return nil
}

// Get returns one observation by stable ID.
func (store *ObservationStore) Get(ctx context.Context, scope sdkmemory.Scope, id string) (Observation, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, err := store.getLocked(ctx, scope, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Observation{}, false, nil
		}
		return Observation{}, false, err
	}
	return value, true, nil
}

// Current returns the newest active observation for one canonical key.
func (store *ObservationStore) Current(ctx context.Context, scope sdkmemory.Scope, key string) (Observation, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.currentLocked(ctx, scope, key)
}

// Events returns every lifecycle event in one scope, ordered by (Time, ID).
func (store *ObservationStore) Events(ctx context.Context, scope sdkmemory.Scope) ([]Event, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.eventsLocked(ctx, scope)
}

// List returns every observation in one scope, ordered by stable ID.
func (store *ObservationStore) List(ctx context.Context, scope sdkmemory.Scope) ([]Observation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	prefix, err := store.observationsPrefix(scope)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	result := make([]Observation, 0, len(entries))
	for _, entry := range entries {
		id, err := observationIDFromKey(prefix, entry.Key)
		if err != nil {
			return nil, err
		}
		value, err := store.getLocked(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (store *ObservationStore) getLocked(ctx context.Context, scope sdkmemory.Scope, id string) (Observation, error) {
	base, err := store.getBaseLocked(ctx, scope, id)
	if err != nil {
		return Observation{}, err
	}
	events, err := store.eventsLocked(ctx, scope)
	if err != nil {
		return Observation{}, err
	}
	for _, event := range events {
		if event.Kind == EventSuperseded && event.ObservationID == id {
			base.State = StateSuperseded
			base.ReplacedBy = event.RelatedID
		}
	}
	return base, nil
}

func (store *ObservationStore) getBaseLocked(ctx context.Context, scope sdkmemory.Scope, id string) (Observation, error) {
	key, err := store.observationKey(scope, id)
	if err != nil {
		return Observation{}, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		return Observation{}, err
	}
	var base Observation
	if err := readEnvelope(data, &base); err != nil {
		return Observation{}, err
	}
	if err := base.State.Validate(); err != nil {
		return Observation{}, err
	}
	return base, nil
}

func (store *ObservationStore) currentLocked(ctx context.Context, scope sdkmemory.Scope, key string) (Observation, bool, error) {
	prefix, err := store.observationsPrefix(scope)
	if err != nil {
		return Observation{}, false, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return Observation{}, false, err
	}
	var result Observation
	found := false
	for _, entry := range entries {
		id, err := observationIDFromKey(prefix, entry.Key)
		if err != nil {
			return Observation{}, false, err
		}
		value, err := store.getLocked(ctx, scope, id)
		if err != nil {
			return Observation{}, false, err
		}
		if value.Key == key && value.State == StateActive && (!found || result.CreatedAt.Before(value.CreatedAt) ||
			(result.CreatedAt.Equal(value.CreatedAt) && result.ID < value.ID)) {
			result, found = value, true
		}
	}
	return result, found, nil
}

func (store *ObservationStore) eventsLocked(ctx context.Context, scope sdkmemory.Scope) ([]Event, error) {
	stream, err := store.eventsStream(scope)
	if err != nil {
		return nil, err
	}
	logEvents, err := store.log.Read(ctx, stream, 0, 0)
	if err != nil {
		return nil, err
	}
	result := make([]Event, 0, len(logEvents))
	for _, event := range logEvents {
		var value Event
		if err := readEnvelope(event.Payload, &value); err != nil {
			return nil, err
		}
		if err := value.Kind.Validate(); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Time.Equal(result[j].Time) {
			return result[i].ID < result[j].ID
		}
		return result[i].Time.Before(result[j].Time)
	})
	return result, nil
}

func (store *ObservationStore) appendEvent(ctx context.Context, event Event) error {
	stream, err := store.eventsStream(event.Scope)
	if err != nil {
		return err
	}
	data, err := json.Marshal(envelope[Event]{SchemaVersion: schemaVersion, Value: event})
	if err != nil {
		return err
	}
	if _, err := store.log.Append(ctx, stream, []storage.Event{{
		Stream:  stream,
		Type:    observationEventType,
		Payload: data,
	}}, storage.AppendOptions{IdempotencyKey: event.ID}); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return errdefs.Conflictf("observation view: immutable event %q conflicts", event.ID)
		}
		return fmt.Errorf("observation view: append event %q: %w", event.ID, err)
	}
	return nil
}

func (store *ObservationStore) putImmutable(ctx context.Context, key string, value any) error {
	data, err := json.Marshal(envelope[any]{SchemaVersion: schemaVersion, Value: value})
	if err != nil {
		return err
	}
	put, ok := store.kv.(storage.PutIfAbsentStore)
	if !ok {
		return errors.New("observation view: store must support immutable writes")
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
		return errdefs.Conflictf("observation view: immutable record conflicts at %q", key)
	}
	return nil
}

func (store *ObservationStore) scopePrefix(scope sdkmemory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "views/observation/v1/" + partition, nil
}

func (store *ObservationStore) observationsPrefix(scope sdkmemory.Scope) (string, error) {
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/observations", nil
}

func (store *ObservationStore) observationKey(scope sdkmemory.Scope, id string) (string, error) {
	prefix, err := store.observationsPrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/" + storage.EncodeSegment(id), nil
}

func (store *ObservationStore) eventsStream(scope sdkmemory.Scope) (string, error) {
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/events", nil
}

func observationIDFromKey(prefix, key string) (string, error) {
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == key {
		return "", errors.New("observation key outside prefix")
	}
	return storage.DecodeSegment(suffix)
}

func readEnvelope(data []byte, destination any) error {
	var raw struct {
		SchemaVersion int             `json:"schema_version"`
		Value         json.RawMessage `json:"value"`
	}
	if err := strictDecode(data, &raw); err != nil || raw.SchemaVersion != schemaVersion {
		if err == nil {
			err = fmt.Errorf("unsupported schema_version %d", raw.SchemaVersion)
		}
		return err
	}
	return strictDecode(raw.Value, destination)
}

func strictDecode(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON")
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
