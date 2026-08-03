package config_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/workspace/config"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestDeployFactorySpec(t *testing.T) {
	got := config.NewDeployFactory().Spec()
	want := deploy.ResourceSpec{
		Kind:     config.ResourceKind,
		Impl:     "yaml",
		ItemType: "workspace.Workspace",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewBuildsRegistryAndRejectsUnknownSettings(t *testing.T) {
	factory := config.NewDeployFactory()
	value, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: settingsNode(t, `
inline:
  version: v1
  workspaces:
    scratch: {driver: memory}
`),
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

	if _, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: settingsNode(t, "unknown: true\n"),
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
}

func settingsNode(t *testing.T, input string) *yamlv3.Node {
	t.Helper()
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(input), &node); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return node.Content[0]
}
