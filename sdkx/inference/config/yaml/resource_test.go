package yaml

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	inferenceconfig "github.com/GizClaw/flowcraft/sdkx/inference/config"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestDeployFactorySpec(t *testing.T) {
	got := NewDeployFactory(nil, nil).Spec()
	want := deploy.ResourceSpec{
		Kind: ResourceKind,
		Impl: "yaml",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewBuildsAssemblyAndRejectsUnknownSettings(t *testing.T) {
	factories := map[string]inferenceconfig.Factory{
		"fake": inferenceconfig.FactoryFunc(func(
			_ context.Context,
			input inferenceconfig.ProviderInput,
		) (inference.ProviderDefinition, error) {
			return inference.ProviderDefinition{
				ID: input.ID,
				Models: []inference.ModelImplementation{{
					Descriptor: inference.ModelDescriptor{
						ID: inference.ModelID{Provider: input.ID, Name: "model"},
					},
					Openers: inference.Openers{
						Generate: func(
							context.Context,
							inference.ModelRef,
						) (inference.GenerateOperations, error) {
							return inference.GenerateOperations{}, nil
						},
					},
				}},
			}, nil
		}),
	}
	factory := NewDeployFactory(factories, nil)
	value, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: inferenceSettingsNode(t, `
inline:
  version: v1
  providers:
    - id: provider
      driver: fake
`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assembly, ok := value.(*inferenceconfig.Assembly)
	if !ok || assembly.Runtime == nil {
		t.Fatalf("New returned %#v, want assembly with runtime", value)
	}

	if _, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: inferenceSettingsNode(t, "unknown: true\n"),
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
}

func inferenceSettingsNode(t *testing.T, input string) *yamlv3.Node {
	t.Helper()
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(input), &node); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return node.Content[0]
}
