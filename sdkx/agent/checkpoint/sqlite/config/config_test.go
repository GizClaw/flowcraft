package config_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sqlitecheckpoint "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/sqlite"
	sqliteconfig "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/sqlite/config"
)

func TestFactorySpec(t *testing.T) {
	spec := sqliteconfig.NewFactory().Spec()
	if spec.Kind != sqliteconfig.ResourceKind || spec.Impl != "sqlite" {
		t.Fatalf("Spec = %+v, want %s/sqlite", spec, sqliteconfig.ResourceKind)
	}
}

func TestFactoryNewPersistsCheckpoints(t *testing.T) {
	factory := sqliteconfig.NewFactory()
	value, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: json.RawMessage(`{"path":":memory:"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store, ok := value.(*sqlitecheckpoint.Store)
	if !ok {
		t.Fatalf("New returned %T, want *sqlite.Store", value)
	}
	defer func() { _ = store.Close() }()

	cp := agent.Checkpoint{
		ExecID: "run-1",
		Steps:  []string{"wave-1"},
		Board:  agent.NewBoard().Snapshot(),
	}
	if err := store.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.ExecID != "run-1" || len(got.Steps) != 1 {
		t.Fatalf("Load = %+v, want run-1 checkpoint", got)
	}
}

func TestFactoryRejectsMissingPath(t *testing.T) {
	factory := sqliteconfig.NewFactory()
	_, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: json.RawMessage(`{}`),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without path = %v, want Validation", err)
	}
}
