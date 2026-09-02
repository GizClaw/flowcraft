package workspace_test

import (
	"context"
	"testing"

	checkpointworkspace "github.com/GizClaw/flowcraft/core/agent/checkpoint/workspace"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func TestRegister(t *testing.T) {
	reg := resource.NewRegistry()
	if err := checkpointworkspace.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup(checkpointworkspace.ResourceKind, "workspace")
	if !ok {
		t.Fatal("checkpoint.Store/workspace factory not registered")
	}
	spec := factory.Spec()
	if len(spec.Deps) != 1 || spec.Deps[0].Name != "workspace" ||
		spec.Deps[0].Type != "workspace.Workspace" || !spec.Deps[0].Required {
		t.Fatalf("spec deps = %+v, want required workspace dep", spec.Deps)
	}

	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"prefix":"custom/ck"}`),
		Deps:     map[string]any{"workspace": ws},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store, ok := value.(*checkpointworkspace.Store)
	if !ok {
		t.Fatalf("New returned %T, want *checkpointworkspace.Store", value)
	}
	if err := store.Save(context.Background(), testCheckpoint("run-1")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	exists, err := ws.Exists(context.Background(), "custom/ck/run-1.json")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("checkpoint not written under settings prefix")
	}
}

func TestFactoryRequiresWorkspaceDep(t *testing.T) {
	_, err := (checkpointworkspace.Factory{}).New(context.Background(), resource.Input{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without workspace dep = %v, want Validation", err)
	}
}

func TestFactoryRejectsWrongDepType(t *testing.T) {
	_, err := (checkpointworkspace.Factory{}).New(context.Background(), resource.Input{
		Deps: map[string]any{"workspace": "not a workspace"},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New with wrong dep type = %v, want Validation", err)
	}
}
