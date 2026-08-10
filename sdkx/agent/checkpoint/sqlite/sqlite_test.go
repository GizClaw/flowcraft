package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	sqlitecheckpoint "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/sqlite"
)

func TestStore_Conformance(t *testing.T) {
	agenttest.CheckpointStoreSuite(t, func() agent.CheckpointStore {
		store, err := sqlitecheckpoint.Open(":memory:")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	})
}

func TestStore_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")

	store, err := sqlitecheckpoint.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cp := testCheckpoint("run-1")
	if err := store.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := sqlitecheckpoint.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || !sameCheckpoint(*got, cp) {
		t.Fatalf("Load = %+v, want %+v", got, cp)
	}
}

func TestStore_RejectsInvalidCheckpoint(t *testing.T) {
	store := mustOpen(t, ":memory:")
	defer func() { _ = store.Close() }()

	err := store.Save(context.Background(), agent.Checkpoint{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("Save(zero cp) = %v, want Validation", err)
	}
}

func TestStore_NewDoesNotCloseCallerDB(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	store, err := sqlitecheckpoint.New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close on non-owned store: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("caller db was closed by store: %v", err)
	}
	_ = db.Close()
}

func mustOpen(t *testing.T, path string) *sqlitecheckpoint.Store {
	t.Helper()
	store, err := sqlitecheckpoint.Open(path)
	if err != nil {
		t.Fatalf("Open %s: %v", path, err)
	}
	return store
}

func testCheckpoint(execID string) agent.Checkpoint {
	return agent.Checkpoint{
		ExecID:            execID,
		Steps:             []string{"wave-1"},
		Iteration:         3,
		Board:             testBoard(),
		Payload:           []byte(`{"task_id":"t1"}`),
		Attributes:        map[string]string{"tenant": "tenant-a"},
		Timestamp:         time.Now().UTC(),
		OriginalStartedAt: time.Now().Add(-time.Hour).UTC(),
		SpecVersion:       "v1",
	}
}

func testBoard() *agent.BoardSnapshot {
	return &agent.BoardSnapshot{
		Vars: map[string]any{
			"x":     float64(1),
			"items": []any{"a", "b"},
		},
		Channels: map[string][]message.Message{
			agent.MainChannel: {message.NewTextMessage(message.RoleAssistant, "hi")},
		},
	}
}

func sameCheckpoint(a, b agent.Checkpoint) bool {
	if !a.Timestamp.Equal(b.Timestamp) || !a.OriginalStartedAt.Equal(b.OriginalStartedAt) {
		return false
	}
	a.Timestamp = time.Time{}
	a.OriginalStartedAt = time.Time{}
	b.Timestamp = time.Time{}
	b.OriginalStartedAt = time.Time{}
	return reflect.DeepEqual(a, b)
}
