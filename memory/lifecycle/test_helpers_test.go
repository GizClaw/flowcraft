package lifecycle

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/storage"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	observationview "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func newFactStore(t *testing.T, ws workspace.Workspace, options ...factview.Option) *factview.FactStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := factview.NewFactStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newObservationStore(t *testing.T, ws workspace.Workspace, options ...observationview.Option) *observationview.ObservationStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := observationview.NewObservationStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newEventStore(t *testing.T, ws workspace.Workspace) *EventStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(logStore)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
