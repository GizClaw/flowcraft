package observation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/storage"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestClosedObservationEnumsValidate(t *testing.T) {
	tests := []struct {
		name     string
		wantErr  bool
		validate func() error
	}{
		{"state active", false, StateActive.Validate},
		{"state superseded", false, StateSuperseded.Validate},
		{"state empty", true, State("").Validate},
		{"state unknown", true, State("unknown").Validate},
		{"state untrimmed", true, State(" active ").Validate},
		{"event integrated", false, EventIntegrated.Validate},
		{"event superseded", false, EventSuperseded.Validate},
		{"event retention", false, EventRetention.Validate},
		{"event visibility", false, EventVisibility.Validate},
		{"event empty", true, EventKind("").Validate},
		{"event unknown", true, EventKind("unknown").Validate},
		{"event untrimmed", true, EventKind(" integrated ").Validate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(); (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestObservationStoreRejectsUnknownPersistedEnums(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	scope := sdkmemory.Scope{RuntimeID: "runtime"}

	t.Run("state", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		kvStore, err := storage.NewWorkspaceKV(ws)
		if err != nil {
			t.Fatal(err)
		}
		store := newObservationStore(t, ws, WithClock(func() time.Time { return now }))
		value, err := store.Integrate(ctx, factview.Fact{ID: "fact", Scope: scope, CreatedAt: now}, "task")
		if err != nil {
			t.Fatal(err)
		}
		key, keyErr := store.observationKey(scope, value.ID)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		data, _ := kvStore.Get(ctx, key)
		if err := kvStore.Put(ctx, key, bytes.Replace(data, []byte(`"state":"active"`), []byte(`"state":"unknown"`), 1)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Get(ctx, scope, value.ID); err == nil {
			t.Fatal("Get accepted an unknown persisted state")
		}
	})

	t.Run("event kind", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		logStore, err := storage.NewWorkspaceLog(ws)
		if err != nil {
			t.Fatal(err)
		}
		store := newObservationStore(t, ws, WithClock(func() time.Time { return now }))
		if _, err := store.Integrate(ctx, factview.Fact{ID: "fact", Scope: scope, CreatedAt: now}, "task"); err != nil {
			t.Fatal(err)
		}
		stream, streamErr := store.eventsStream(scope)
		if streamErr != nil {
			t.Fatal(streamErr)
		}
		payload, err := json.Marshal(envelope[Event]{
			SchemaVersion: schemaVersion,
			Value: Event{ID: "bad", Scope: scope, Kind: "unknown", ObservationID: "fact",
				TaskID: "task", Time: now},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := logStore.Append(ctx, stream, []storage.Event{{
			Stream: stream, Type: observationEventType, Payload: payload,
		}}, storage.AppendOptions{IdempotencyKey: "bad"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Events(ctx, scope); err == nil {
			t.Fatal("Events accepted an unknown persisted kind")
		}
	})
}

func TestIntegrateConflictReplacementAndReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	store := newObservationStore(t, workspace.NewMemWorkspace(), WithClock(func() time.Time { return now }))
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	first := factview.Fact{ID: "old", Scope: scope, ConversationID: "c", Entities: []string{"alice"}, Predicate: "city", CreatedAt: now}
	second := factview.Fact{ID: "new", Scope: scope, ConversationID: "c", Entities: []string{"alice"}, Predicate: "city", CreatedAt: now.Add(time.Second)}

	old, err := store.Integrate(ctx, first, "task-old")
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Integrate(ctx, second, "task-new")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Integrate(ctx, second, "task-new")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != current.ID || current.Replaces != old.ID {
		t.Fatalf("replacement/replay = %#v, %#v, %#v", old, current, replayed)
	}
	previous, ok, err := store.Get(ctx, scope, old.ID)
	if err != nil || !ok || previous.State != StateSuperseded || previous.ReplacedBy != current.ID {
		t.Fatalf("old state = %#v, %v, %v", previous, ok, err)
	}
	events, err := store.Events(ctx, scope)
	if err != nil || len(events) != 3 {
		t.Fatalf("events = %#v, %v", events, err)
	}
}

func TestIntegrateSameFactWithDistinctTasksMaintainsSingleReplacementChain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	clock := now
	store := newObservationStore(t, workspace.NewMemWorkspace(), WithClock(func() time.Time { return clock }))
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	fact := factview.Fact{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Entities: []string{"alice"}, Predicate: "city", CreatedAt: now,
	}

	first, err := store.Integrate(ctx, fact, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	second, err := store.Integrate(ctx, fact, "task-2")
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	third, err := store.Integrate(ctx, fact, "task-3")
	if err != nil {
		t.Fatal(err)
	}
	if second.Replaces != first.ID || third.Replaces != second.ID {
		t.Fatalf("replacement chain = %q <- %q <- %q", first.Replaces, second.Replaces, third.Replaces)
	}
	values, err := store.List(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, value := range values {
		if value.State == StateActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active observations = %d, want 1: %#v", active, values)
	}
	if err := ValidateReplacementChain(values); err != nil {
		t.Fatalf("replacement chain invalid: %v", err)
	}
	replayed, err := store.Integrate(ctx, fact, "task-3")
	if err != nil || replayed.ID != third.ID || replayed.Replaces != second.ID {
		t.Fatalf("idempotent replay = %#v, %v", replayed, err)
	}
}

func TestIntegrateFallbackKeyAndRejectsReplacementCycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	store := newObservationStore(t, workspace.NewMemWorkspace(), WithClock(func() time.Time { return now }))
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	value := factview.Fact{ID: "fact", Scope: scope, ConversationID: "c", CanonicalHash: "hash", CreatedAt: now}
	got, err := store.Integrate(ctx, value, "task")
	if err != nil || got.Key != "fallback:hash" {
		t.Fatalf("fallback = %#v, %v", got, err)
	}
	if err := ValidateReplacementChain([]Observation{{ID: "a", Replaces: "a"}}); err == nil {
		t.Fatal("self replacement accepted")
	}
	if err := ValidateReplacementChain([]Observation{{ID: "a", Replaces: "b"}, {ID: "b", Replaces: "a"}}); err == nil {
		t.Fatal("cycle accepted")
	}
}

func TestIntegrateRetryReusesDurableObservationAfterEventFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	clock := now
	ws := workspace.NewMemWorkspace()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	failing := &failAppendLog{Log: logStore}
	store, err := NewObservationStore(failing, kvStore, WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	fact := factview.Fact{
		ID: "fact", Scope: scope, ConversationID: "conversation", CanonicalHash: "hash",
		EventTime: now.Add(-time.Hour), CreatedAt: now,
	}
	if _, err := store.Integrate(ctx, fact, "task"); err == nil {
		t.Fatal("injected post-observation event failure succeeded")
	}
	clock = now.Add(time.Hour)
	reopened, err := NewObservationStore(logStore, kvStore, WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Integrate(ctx, fact, "task")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("retry rewrote immutable created_at: %v", got.CreatedAt)
	}
	events, err := reopened.Events(ctx, scope)
	if err != nil || len(events) != 1 || !events[0].Time.Equal(now) {
		t.Fatalf("repaired events=%#v err=%v", events, err)
	}
}

func newObservationStore(t *testing.T, ws workspace.Workspace, options ...Option) *ObservationStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewObservationStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type failAppendLog struct {
	storage.Log
	failed bool
}

func (log *failAppendLog) Append(
	ctx context.Context,
	stream string,
	events []storage.Event,
	options storage.AppendOptions,
) (storage.Commit, error) {
	if !log.failed && strings.Contains(stream, "/events") {
		log.failed = true
		return storage.Commit{}, errors.New("injected event publish failure")
	}
	return log.Log.Append(ctx, stream, events, options)
}
