// Package observation stores immutable lifecycle observations and events.
package observation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const schemaVersion = 1

type State string

const (
	StateActive     State = "active"
	StateSuperseded State = "superseded"
)

func (state State) Validate() error {
	switch state {
	case StateActive, StateSuperseded:
		return nil
	default:
		return fmt.Errorf("observation view: unknown state %q", state)
	}
}

type EventKind string

const (
	EventIntegrated EventKind = "integrated"
	EventSuperseded EventKind = "superseded"
	EventRetention  EventKind = "retention_scored"
	EventVisibility EventKind = "visibility_changed"
)

func (kind EventKind) Validate() error {
	switch kind {
	case EventIntegrated, EventSuperseded, EventRetention, EventVisibility:
		return nil
	default:
		return fmt.Errorf("observation view: unknown event kind %q", kind)
	}
}

type Observation struct {
	ID             string                `json:"id"`
	Scope          sdkmemory.Scope       `json:"scope"`
	Key            string                `json:"key"`
	FactID         string                `json:"fact_id"`
	ConversationID string                `json:"conversation_id"`
	State          State                 `json:"state"`
	Replaces       string                `json:"replaces,omitempty"`
	ReplacedBy     string                `json:"replaced_by,omitempty"`
	Provenance     []sdkmemory.SourceRef `json:"provenance,omitempty"`
	SourceDigest   string                `json:"source_digest,omitempty"`
	EventTime      time.Time             `json:"event_time,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
}

type Event struct {
	ID            string          `json:"id"`
	Scope         sdkmemory.Scope `json:"scope"`
	Kind          EventKind       `json:"kind"`
	ObservationID string          `json:"observation_id"`
	RelatedID     string          `json:"related_id,omitempty"`
	TaskID        string          `json:"task_id"`
	Time          time.Time       `json:"time"`
}

type Option func(*WorkspaceStore)

func WithClock(clock func() time.Time) Option {
	return func(store *WorkspaceStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

type WorkspaceStore struct {
	ws    workspace.Workspace
	clock func() time.Time
	mu    sync.Mutex
}

type envelope[T any] struct {
	SchemaVersion int `json:"schema_version"`
	Value         T   `json:"value"`
}

func NewWorkspaceStore(ws workspace.Workspace, options ...Option) (*WorkspaceStore, error) {
	if ws == nil {
		return nil, errors.New("observation view: workspace is required")
	}
	store := &WorkspaceStore{ws: ws, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

func CanonicalKey(value factview.Fact) string {
	entities := factview.NormalizeEntities(value.Entities)
	predicate := factview.CanonicalContent(value.Predicate)
	if len(entities) > 0 && predicate != "" {
		return "entity-predicate:" + strings.Join(entities, "\x1f") + ":" + predicate
	}
	fallback := strings.TrimSpace(value.CanonicalHash)
	if fallback == "" {
		fallback = value.ID
	}
	return "fallback:" + fallback
}

// Integrate appends immutable records. Replaying a task repairs a partial
// supersede sequence and returns the same observation.
func (store *WorkspaceStore) Integrate(ctx context.Context, value factview.Fact, taskID string) (Observation, error) {
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
	} else if !errdefs.IsNotFound(err) {
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
	if err := store.putImmutable(ctx, store.observationPath(value.Scope, id), observation); err != nil {
		return Observation{}, err
	}
	if err := store.finishIntegration(ctx, observation, taskID); err != nil {
		return Observation{}, err
	}
	return store.getLocked(ctx, value.Scope, id)
}

func (store *WorkspaceStore) finishIntegration(ctx context.Context, observation Observation, taskID string) error {
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

func (store *WorkspaceStore) Get(ctx context.Context, scope sdkmemory.Scope, id string) (Observation, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, err := store.getLocked(ctx, scope, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Observation{}, false, nil
		}
		return Observation{}, false, err
	}
	return value, true, nil
}

func (store *WorkspaceStore) Current(ctx context.Context, scope sdkmemory.Scope, key string) (Observation, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.currentLocked(ctx, scope, key)
}

func (store *WorkspaceStore) Events(ctx context.Context, scope sdkmemory.Scope) ([]Event, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.eventsLocked(ctx, scope)
}

func (store *WorkspaceStore) List(ctx context.Context, scope sdkmemory.Scope) ([]Observation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := store.ws.List(ctx, store.observationsDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []Observation{}, nil
		}
		return nil, err
	}
	result := make([]Observation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id, err := decode(strings.TrimSuffix(entry.Name(), ".json"))
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

func (store *WorkspaceStore) getLocked(ctx context.Context, scope sdkmemory.Scope, id string) (Observation, error) {
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

func (store *WorkspaceStore) getBaseLocked(ctx context.Context, scope sdkmemory.Scope, id string) (Observation, error) {
	var base Observation
	if err := store.readEnvelope(ctx, store.observationPath(scope, id), &base); err != nil {
		return Observation{}, err
	}
	if err := base.State.Validate(); err != nil {
		return Observation{}, err
	}
	return base, nil
}

func (store *WorkspaceStore) currentLocked(ctx context.Context, scope sdkmemory.Scope, key string) (Observation, bool, error) {
	entries, err := store.ws.List(ctx, store.observationsDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Observation{}, false, nil
		}
		return Observation{}, false, err
	}
	var result Observation
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id, err := decode(strings.TrimSuffix(entry.Name(), ".json"))
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

func (store *WorkspaceStore) eventsLocked(ctx context.Context, scope sdkmemory.Scope) ([]Event, error) {
	entries, err := store.ws.List(ctx, store.eventsDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []Event{}, nil
		}
		return nil, err
	}
	result := make([]Event, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var event Event
		if err := store.readEnvelope(ctx, path.Join(store.eventsDir(scope), entry.Name()), &event); err != nil {
			return nil, err
		}
		if err := event.Kind.Validate(); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Time.Equal(result[j].Time) {
			return result[i].ID < result[j].ID
		}
		return result[i].Time.Before(result[j].Time)
	})
	return result, nil
}

func (store *WorkspaceStore) appendEvent(ctx context.Context, event Event) error {
	return store.putImmutable(ctx, path.Join(store.eventsDir(event.Scope), encode(event.ID)+".json"), event)
}

func (store *WorkspaceStore) putImmutable(ctx context.Context, target string, value any) error {
	data, err := json.Marshal(envelope[any]{SchemaVersion: schemaVersion, Value: value})
	if err != nil {
		return err
	}
	existing, err := store.ws.Read(ctx, target)
	if err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return errdefs.Conflictf("observation view: immutable record conflicts at %q", target)
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	return workspace.AtomicWrite(ctx, store.ws, target, data)
}

func (store *WorkspaceStore) readEnvelope(ctx context.Context, target string, destination any) error {
	data, err := store.ws.Read(ctx, target)
	if err != nil {
		return err
	}
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

func ValidateReplacementChain(values []Observation) error {
	parents := make(map[string]string, len(values))
	for _, value := range values {
		if value.ID == "" {
			return errors.New("observation view: replacement id is required")
		}
		if value.Replaces == value.ID {
			return errors.New("observation view: self replacement")
		}
		parents[value.ID] = value.Replaces
	}
	for id := range parents {
		seen := map[string]struct{}{}
		for current := id; current != ""; current = parents[current] {
			if _, duplicate := seen[current]; duplicate {
				return errors.New("observation view: replacement cycle")
			}
			seen[current] = struct{}{}
		}
	}
	return nil
}

func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (store *WorkspaceStore) root(scope sdkmemory.Scope) string {
	return path.Join("views", "observation", "v1", "partitions", encode(scope.RuntimeID), encode(scope.UserID), encode(scope.AgentID))
}
func (store *WorkspaceStore) observationsDir(scope sdkmemory.Scope) string {
	return path.Join(store.root(scope), "observations")
}
func (store *WorkspaceStore) observationPath(scope sdkmemory.Scope, id string) string {
	return path.Join(store.observationsDir(scope), encode(id)+".json")
}
func (store *WorkspaceStore) eventsDir(scope sdkmemory.Scope) string {
	return path.Join(store.root(scope), "events")
}
func encode(value string) string { return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value)) }
func decode(value string) (string, error) {
	if !strings.HasPrefix(value, "k_") {
		return "", errors.New("observation view: invalid path segment")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "k_"))
	if err != nil || encode(string(data)) != value {
		return "", errors.New("observation view: invalid path segment")
	}
	return string(data), nil
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
