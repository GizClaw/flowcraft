package config_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdk/workspace/config"
)

func TestDeployFactorySpec(t *testing.T) {
	got := config.NewDeployFactory(config.NewBuilder(config.Deps{})).Spec()
	want := sdkconfig.ResourceSpec{
		Kind:     config.ResourceKind,
		Impl:     "yaml",
		ItemType: "workspace.Workspace",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewBuildsRegistryAndRejectsUnknownSettings(t *testing.T) {
	factory := config.NewDeployFactory(config.NewBuilder(config.Deps{}))
	value, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsOpaque(t, `{
			"inline": {
				"version": "v1",
				"workspaces": {"scratch": {"driver": "memory"}}
			}
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registry, ok := value.(*config.Registry)
	if !ok {
		t.Fatalf("New returned %T, want *config.Registry", value)
	}
	item, ok := registry.ResolveItem("scratch")
	if !ok {
		t.Fatal("ResolveItem(scratch) did not find workspace")
	}
	if _, ok := item.(workspace.Workspace); !ok {
		t.Fatalf("ResolveItem returned %T, want workspace.Workspace", item)
	}

	if _, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsOpaque(t, `{"unknown": true}`),
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
}

func TestDeployFactoryUsesHostBuilderDrivers(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	if err := builder.RegisterFactory("custom", func(_ context.Context, in sdkconfig.Input) (config.Resource, error) {
		return config.Resource{Workspace: workspace.NewMemWorkspace()}, nil
	}); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	factory := config.NewDeployFactory(builder)
	value, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsOpaque(t, `{
			"inline": {
				"version": "v1",
				"workspaces": {"scratch": {"driver": "custom"}}
			}
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registry, ok := value.(*config.Registry)
	if !ok {
		t.Fatalf("New returned %T, want *config.Registry", value)
	}
	if _, ok := registry.Get("scratch"); !ok {
		t.Fatal("custom driver workspace missing from registry")
	}
}

func TestDeployFactoryRejectsNilBuilder(t *testing.T) {
	factory := config.NewDeployFactory(nil)
	if _, err := factory.New(context.Background(), sdkconfig.Input{}); err == nil {
		t.Fatal("New accepted a nil builder")
	}
}

func settingsOpaque(t *testing.T, raw string) *sdkconfig.Opaque {
	t.Helper()
	var opaque sdkconfig.Opaque
	if err := json.Unmarshal([]byte(raw), &opaque); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return &opaque
}
