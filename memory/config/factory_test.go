package config

import (
	"context"
	"strings"
	"testing"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestFactoryRejectsMissingDeps(t *testing.T) {
	factory := Factory()
	if _, err := factory.New(context.Background(), sdkmemory.Input{}); err == nil {
		t.Fatal("New succeeded without deps")
	} else if !strings.Contains(err.Error(), "inference") {
		t.Fatalf("missing inference error = %v", err)
	}
	runtime, _, _ := testRuntime(t)
	if _, err := factory.New(context.Background(), sdkmemory.Input{
		Deps: map[string]any{"inference": runtime},
	}); err == nil {
		t.Fatal("New succeeded without workspace")
	} else if !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("missing workspace error = %v", err)
	}
}

func TestFactoryRejectsWrongDepTypes(t *testing.T) {
	factory := Factory()
	if _, err := factory.New(context.Background(), sdkmemory.Input{
		Deps: map[string]any{
			"inference": "not a runtime",
			"workspace": workspace.NewMemWorkspace(),
		},
	}); err == nil {
		t.Fatal("New accepted a non-runtime inference dep")
	}
	runtime, _, _ := testRuntime(t)
	if _, err := factory.New(context.Background(), sdkmemory.Input{
		Deps: map[string]any{
			"inference": runtime,
			"workspace": "not a workspace",
		},
	}); err == nil {
		t.Fatal("New accepted a non-workspace workspace dep")
	}
}

func TestFactoryBuildsAssemblyFromDeps(t *testing.T) {
	runtime, _, _ := testRuntime(t)
	// Wire-format settings: durations are strings, so marshal through the
	// JSON protocol instead of the Go struct (Duration only unmarshals).
	settings := []byte(`{
		"generate": {"provider": "fake", "name": "generate"},
		"embed": {"provider": "fake", "name": "embed"},
		"scopes": [{"runtime_id": "memory", "user_id": "tenant"}]
	}`)
	assembly, err := Factory().New(context.Background(), sdkmemory.Input{
		Settings: settings,
		Deps: map[string]any{
			"inference": runtime,
			"workspace": workspace.NewMemWorkspace(),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	built, ok := assembly.(*Assembly)
	if !ok {
		t.Fatalf("New returned %T, want *Assembly", assembly)
	}
	defer built.Close()
	if built.System == nil {
		t.Fatal("assembly has no system")
	}
}
