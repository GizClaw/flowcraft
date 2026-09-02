package workspace_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	checkpointworkspace "github.com/GizClaw/flowcraft/core/agent/checkpoint/workspace"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func newStore(t *testing.T) *checkpointworkspace.Store {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	store, err := checkpointworkspace.New(ws)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func TestStore_Conformance(t *testing.T) {
	agenttest.CheckpointStoreSuite(t, func() agent.CheckpointStore {
		return newStore(t)
	})
}

func TestStore_SharedWorkspacePersistsAcrossInstances(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	first, err := checkpointworkspace.New(ws)
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	second, err := checkpointworkspace.New(ws)
	if err != nil {
		t.Fatalf("New second: %v", err)
	}

	cp := testCheckpoint("run-1")
	if err := first.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := second.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.ExecID != "run-1" {
		t.Fatalf("Load = %+v, want run-1", got)
	}
}

func TestStore_PrefixIsolation(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	store, err := checkpointworkspace.New(ws, checkpointworkspace.WithPrefix("custom/ck"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.Save(context.Background(), testCheckpoint("run-1")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	exists, err := ws.Exists(context.Background(), "custom/ck/run-1.json")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("checkpoint not written under configured prefix")
	}
	exists, err = ws.Exists(context.Background(), "agent/checkpoints/run-1.json")
	if err != nil {
		t.Fatalf("Exists default: %v", err)
	}
	if exists {
		t.Fatal("checkpoint leaked into default prefix")
	}
}

func TestStore_RejectsInvalidCheckpoint(t *testing.T) {
	store := newStore(t)
	if err := store.Save(context.Background(), agent.Checkpoint{}); !errdefs.IsValidation(err) {
		t.Fatalf("Save(zero cp) = %v, want Validation", err)
	}
}

func testCheckpoint(execID string) agent.Checkpoint {
	return agent.Checkpoint{
		ExecID:     execID,
		Steps:      []string{"wave-1"},
		Iteration:  1,
		Board:      agent.NewBoard().Snapshot(),
		Attributes: map[string]string{"tenant": "tenant-a"},
	}
}
