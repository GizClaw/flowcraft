package config_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	checkpointworkspace "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/workspace"
	workspaceconfig "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/workspace/config"
)

func TestFactorySpec(t *testing.T) {
	spec := workspaceconfig.NewFactory().Spec()
	if spec.Kind != workspaceconfig.ResourceKind || spec.Impl != "workspace" {
		t.Fatalf("Spec = %+v, want %s/workspace", spec, workspaceconfig.ResourceKind)
	}
	if len(spec.Deps) != 1 || spec.Deps[0].Name != "workspace" ||
		spec.Deps[0].Type != "workspace.Workspace" || !spec.Deps[0].Required {
		t.Fatalf("Spec deps = %+v, want required workspace dep", spec.Deps)
	}
}

func TestFactoryNewBindsWorkspace(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	value, err := workspaceconfig.NewFactory().New(context.Background(), sdkconfig.Input{
		Deps:     map[string]any{"workspace": ws},
		Settings: json.RawMessage(`{"prefix":"ck"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store, ok := value.(*checkpointworkspace.Store)
	if !ok {
		t.Fatalf("New returned %T, want *workspace.Store", value)
	}
	cp := agent.Checkpoint{
		ExecID: "run-1",
		Steps:  []string{"wave-1"},
		Board:  agent.NewBoard().Snapshot(),
	}
	if err := store.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	exists, err := ws.Exists(context.Background(), "ck/run-1.json")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("checkpoint not written under configured prefix")
	}
}

func TestFactoryRejectsMissingWorkspace(t *testing.T) {
	_, err := workspaceconfig.NewFactory().New(context.Background(), sdkconfig.Input{
		Settings: json.RawMessage(`{}`),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without workspace = %v, want Validation", err)
	}
}
