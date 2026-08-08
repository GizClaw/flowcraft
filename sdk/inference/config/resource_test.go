package config_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/inference"
	inferenceconfig "github.com/GizClaw/flowcraft/sdk/inference/config"
)

func TestDeployFactorySpec(t *testing.T) {
	got := inferenceconfig.NewDeployFactory(nil, nil).Spec()
	want := sdkconfig.ResourceSpec{
		Kind:     inferenceconfig.ResourceKind,
		Impl:     "yaml",
		ItemType: "inference.Runtime",
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
	factory := inferenceconfig.NewDeployFactory(factories, nil)
	value, err := factory.New(context.Background(), sdkconfig.Input{
		Resolve: resolveLiteral(t),
		Settings: literalSettings(t, `{
			"version": "v1",
			"providers": [
				{"id": "provider", "driver": "fake"}
			]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assembly, ok := value.(*inferenceconfig.Assembly)
	if !ok || assembly.Runtime == nil {
		t.Fatalf("New returned %#v, want assembly with runtime", value)
	}
	if item, ok := assembly.ResolveItem("runtime"); !ok || item != assembly.Runtime {
		t.Fatalf("ResolveItem(runtime) = (%T, %v), want runtime", item, ok)
	}
	if item, ok := assembly.ResolveItem("missing"); ok || item != nil {
		t.Fatalf("ResolveItem(missing) = (%#v, %v), want nil, false", item, ok)
	}

	if _, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsJSON(t, `{"unknown": true}`),
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
}

func settingsJSON(t *testing.T, raw string) *sdkconfig.Opaque {
	t.Helper()
	var opaque sdkconfig.Opaque
	if err := json.Unmarshal([]byte(raw), &opaque); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return &opaque
}

func literalSettings(t *testing.T, doc string) *sdkconfig.Opaque {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal literal settings: %v", err)
	}
	var opaque sdkconfig.Opaque
	if err := json.Unmarshal(raw, &opaque); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return &opaque
}

func resolveLiteral(t *testing.T) func(context.Context, sdkconfig.Source) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, src sdkconfig.Source) ([]byte, error) {
		return sdkconfig.NewLoader().Load(ctx, src)
	}
}
