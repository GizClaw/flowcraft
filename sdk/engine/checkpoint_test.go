package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

func TestNoopCheckpointStore_SaveLoad(t *testing.T) {
	var s engine.NoopCheckpointStore

	cp := engine.Checkpoint{
		ExecID:    "run-1",
		Step:      "node-2",
		Iteration: 3,
		Board:     engine.NewBoard().Snapshot(),
		Timestamp: time.Now(),
	}
	if err := s.Save(context.Background(), cp); err != nil {
		t.Errorf("Save returned error: %v", err)
	}

	got, err := s.Load(context.Background(), "run-1")
	if err != nil {
		t.Errorf("Load returned error: %v", err)
	}
	if got != nil {
		t.Errorf("Noop Load must return nil, nil; got %+v", got)
	}
}

func TestCheckpoint_StoreInterfaces(t *testing.T) {
	// Compile-time assertion that NoopCheckpointStore satisfies the
	// CheckpointStore contract (it does not implement the optional
	// CheckpointLister / CheckpointDeleter shoulds).
	var _ engine.CheckpointStore = engine.NoopCheckpointStore{}
}
