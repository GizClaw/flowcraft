package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const lifecycleEventSchemaVersion = 1

const (
	recallEventType     = "lifecycle.recall"
	visibilityEventType = "lifecycle.visibility"
)

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

// EventStore persists recall and visibility lifecycle events as appends in a
// storage.Log. Aggregates (Access, Visible) are reduced from the Log so the
// Log is the single source of truth.
type EventStore struct {
	log storage.Log
	mu  sync.Mutex
}

type recallEnvelope struct {
	SchemaVersion int                   `json:"schema_version"`
	Event         sdkmemory.RecallEvent `json:"event"`
}
type visibilityEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Event         VisibilityEvent `json:"event"`
}

// NewEventStore constructs a Log-backed lifecycle event store.
func NewEventStore(log storage.Log) (*EventStore, error) {
	if nilStore(log) {
		return nil, errors.New("memory lifecycle events: log is required")
	}
	return &EventStore{log: log}, nil
}

// RecordRecall appends one immutable recall event.
func (store *EventStore) RecordRecall(ctx context.Context, event sdkmemory.RecallEvent) error {
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
	return store.append(ctx, event.Scope, recallEventType, event.ID, data)
}

// Access reduces recall events into one per-item aggregate.
func (store *EventStore) Access(ctx context.Context, scope sdkmemory.Scope, itemID string) (AccessAggregate, bool, error) {
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

// SetSoftForgotten appends one immutable visibility overlay event.
func (store *EventStore) SetSoftForgotten(ctx context.Context, event VisibilityEvent) error {
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
	return store.append(ctx, event.Scope, visibilityEventType, event.ID, data)
}

// Visible implements retrieval.Visibility. With no overlay event, items are
// visible by default.
func (store *EventStore) Visible(ctx context.Context, scope sdkmemory.Scope, itemID string) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	stream, err := store.stream(scope, visibilityEventType)
	if err != nil {
		return false, err
	}
	logEvents, err := store.log.Read(ctx, stream, 0, 0)
	if err != nil {
		return false, err
	}
	var latest VisibilityEvent
	for _, logEvent := range logEvents {
		var value visibilityEnvelope
		if err := decodeLifecycleEvent(logEvent.Payload, &value); err != nil {
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

func (store *EventStore) recallEvents(ctx context.Context, scope sdkmemory.Scope) ([]sdkmemory.RecallEvent, error) {
	stream, err := store.stream(scope, recallEventType)
	if err != nil {
		return nil, err
	}
	logEvents, err := store.log.Read(ctx, stream, 0, 0)
	if err != nil {
		return nil, err
	}
	result := make([]sdkmemory.RecallEvent, 0, len(logEvents))
	for _, logEvent := range logEvents {
		var value recallEnvelope
		if err := decodeLifecycleEvent(logEvent.Payload, &value); err != nil {
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

func (store *EventStore) append(ctx context.Context, scope sdkmemory.Scope, eventType, id string, payload []byte) error {
	stream, err := store.stream(scope, eventType)
	if err != nil {
		return err
	}
	if _, err := store.log.Append(ctx, stream, []storage.Event{{
		Stream:  stream,
		Type:    eventType,
		Payload: payload,
	}}, storage.AppendOptions{IdempotencyKey: id}); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return errdefs.Conflictf("memory lifecycle events: immutable event conflict")
		}
		return err
	}
	return nil
}

func (store *EventStore) stream(scope sdkmemory.Scope, eventType string) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "events/v1/" + partition + "/" + eventType, nil
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
