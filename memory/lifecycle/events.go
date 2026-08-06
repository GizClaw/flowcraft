package lifecycle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const lifecycleEventSchemaVersion = 1

type AccessAggregate struct {
	ItemID          string
	AccessCount     uint64
	LastRecallAt    time.Time
	LastRecallScore float64
}

type VisibilityEvent struct {
	ID            string          `json:"id"`
	Scope         sdkmemory.Scope `json:"scope"`
	ObservationID string          `json:"observation_id"`
	SoftForgotten bool            `json:"soft_forgotten"`
	PlanID        string          `json:"plan_id"`
	Time          time.Time       `json:"time"`
}

type WorkspaceEventStore struct {
	ws workspace.Workspace
	mu sync.Mutex
}

type recallEnvelope struct {
	SchemaVersion int                   `json:"schema_version"`
	Event         sdkmemory.RecallEvent `json:"event"`
}
type visibilityEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Event         VisibilityEvent `json:"event"`
}

func NewWorkspaceEventStore(ws workspace.Workspace) (*WorkspaceEventStore, error) {
	if ws == nil {
		return nil, errors.New("memory lifecycle events: workspace is required")
	}
	return &WorkspaceEventStore{ws: ws}, nil
}

func (store *WorkspaceEventStore) RecordRecall(ctx context.Context, event sdkmemory.RecallEvent) error {
	event.ItemIDs = append([]string(nil), event.ItemIDs...)
	event.Scores = append([]float64(nil), event.Scores...)
	if err := event.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(recallEnvelope{SchemaVersion: lifecycleEventSchemaVersion, Event: event})
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.putImmutable(ctx, store.recallPath(event.Scope, event.ID), data)
}

func (store *WorkspaceEventStore) Access(ctx context.Context, scope sdkmemory.Scope, itemID string) (AccessAggregate, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	events, err := store.recallEvents(ctx, scope)
	if err != nil {
		return AccessAggregate{}, false, err
	}
	var result AccessAggregate
	for _, event := range events {
		index := sort.SearchStrings(event.ItemIDs, itemID)
		if index >= len(event.ItemIDs) || event.ItemIDs[index] != itemID {
			continue
		}
		result.ItemID = itemID
		result.AccessCount++
		if result.LastRecallAt.IsZero() || !event.Time.Before(result.LastRecallAt) {
			result.LastRecallAt, result.LastRecallScore = event.Time, event.Scores[index]
		}
	}
	return result, result.AccessCount > 0, nil
}

func (store *WorkspaceEventStore) SetSoftForgotten(ctx context.Context, event VisibilityEvent) error {
	if err := event.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.ObservationID) == "" ||
		strings.TrimSpace(event.PlanID) == "" || event.Time.IsZero() {
		return errors.New("memory lifecycle events: visibility identity, plan, and time are required")
	}
	data, err := json.Marshal(visibilityEnvelope{SchemaVersion: lifecycleEventSchemaVersion, Event: event})
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.putImmutable(ctx, store.visibilityPath(event.Scope, event.ID), data)
}

// Visible implements retrieval.Visibility. With no overlay event, items are
// visible by default.
func (store *WorkspaceEventStore) Visible(ctx context.Context, scope sdkmemory.Scope, itemID string) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := store.ws.List(ctx, store.visibilityDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	var latest VisibilityEvent
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := store.ws.Read(ctx, path.Join(store.visibilityDir(scope), entry.Name()))
		if err != nil {
			return false, err
		}
		var value visibilityEnvelope
		if err := decodeLifecycleEvent(data, &value); err != nil {
			return false, err
		}
		if value.SchemaVersion != lifecycleEventSchemaVersion {
			return false, errors.New("memory lifecycle events: unsupported schema")
		}
		if value.Event.ObservationID == itemID && (latest.Time.IsZero() || latest.Time.Before(value.Event.Time)) {
			latest = value.Event
		}
	}
	if latest.Time.IsZero() {
		return true, nil
	}
	return !latest.SoftForgotten, nil
}

func (store *WorkspaceEventStore) recallEvents(ctx context.Context, scope sdkmemory.Scope) ([]sdkmemory.RecallEvent, error) {
	entries, err := store.ws.List(ctx, store.recallDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []sdkmemory.RecallEvent{}, nil
		}
		return nil, err
	}
	result := make([]sdkmemory.RecallEvent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := store.ws.Read(ctx, path.Join(store.recallDir(scope), entry.Name()))
		if err != nil {
			return nil, err
		}
		var value recallEnvelope
		if err := decodeLifecycleEvent(data, &value); err != nil {
			return nil, err
		}
		if value.SchemaVersion != lifecycleEventSchemaVersion {
			return nil, errors.New("memory lifecycle events: unsupported schema")
		}
		if err := value.Event.Validate(); err != nil {
			return nil, err
		}
		result = append(result, value.Event)
	}
	return result, nil
}

func (store *WorkspaceEventStore) putImmutable(ctx context.Context, target string, data []byte) error {
	existing, err := store.ws.Read(ctx, target)
	if err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return errdefs.Conflictf("memory lifecycle events: immutable event conflict")
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	return workspace.AtomicWrite(ctx, store.ws, target, data)
}

func (store *WorkspaceEventStore) root(scope sdkmemory.Scope) string {
	return path.Join("events", "memory-lifecycle", "v1", "partitions", encodeLifecycle(scope.RuntimeID), encodeLifecycle(scope.UserID), encodeLifecycle(scope.AgentID))
}
func (store *WorkspaceEventStore) recallDir(scope sdkmemory.Scope) string {
	return path.Join(store.root(scope), "recall")
}
func (store *WorkspaceEventStore) recallPath(scope sdkmemory.Scope, id string) string {
	return path.Join(store.recallDir(scope), encodeLifecycle(id)+".json")
}
func (store *WorkspaceEventStore) visibilityDir(scope sdkmemory.Scope) string {
	return path.Join(store.root(scope), "visibility")
}
func (store *WorkspaceEventStore) visibilityPath(scope sdkmemory.Scope, id string) string {
	return path.Join(store.visibilityDir(scope), encodeLifecycle(id)+".json")
}
func encodeLifecycle(value string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value))
}
func decodeLifecycleEvent(data []byte, destination any) error {
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
