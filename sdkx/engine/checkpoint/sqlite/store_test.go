package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdkx/engine/checkpoint/conformance"
	"github.com/GizClaw/flowcraft/sdkx/engine/checkpoint/sqlite"
)

func TestStoreContract(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) engine.CheckpointStore {
		dir := t.TempDir()
		dsn := "file:" + filepath.Join(dir, "ckpt.db") + "?_pragma=journal_mode(WAL)"
		s, err := sqlite.Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestOpenInMemory(t *testing.T) {
	s, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer s.Close()

	if err := s.Migrate(context.Background()); err != nil {
		t.Errorf("Migrate idempotency: %v", err)
	}
}

func TestWithCustomTable(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "ckpt.db")
	s, err := sqlite.Open(context.Background(), dsn, sqlite.WithTable("vessel_x_ckpts"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	board := engine.NewBoard()
	cp := engine.Checkpoint{ExecID: "r1", Board: board.Snapshot()}
	if err := s.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(context.Background(), "r1")
	if err != nil || got == nil {
		t.Fatalf("Load: got=%v err=%v", got, err)
	}
}
