package config_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/tool/config"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestDeployFactorySpec(t *testing.T) {
	got := config.NewDeployFactory(
		config.NewBuilder(tool.NewRegistry(), config.Deps{}),
	).Spec()
	want := deploy.ResourceSpec{
		Kind: config.ResourceKind,
		Impl: "yaml",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewBuildsAssemblyAndRejectsInvalidInput(t *testing.T) {
	factory := config.NewDeployFactory(
		config.NewBuilder(tool.NewRegistry(), config.Deps{}),
	)
	value, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: toolSettingsNode(t, `
inline:
  version: v1
`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(*config.Assembly); !ok {
		t.Fatalf("New returned %T, want *config.Assembly", value)
	}

	if _, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: toolSettingsNode(t, "unknown: true\n"),
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
	if _, err := config.NewDeployFactory(nil).New(
		context.Background(),
		deploy.ResourceInput{},
	); err == nil {
		t.Fatal("New with nil tool builder succeeded")
	}
}

func toolSettingsNode(t *testing.T, input string) *yamlv3.Node {
	t.Helper()
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(input), &node); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return node.Content[0]
}
