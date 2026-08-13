package inference_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

type fakeProviderFactory struct{ name string }

func (f fakeProviderFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "inference.Provider", Impl: f.name}
}

func (f fakeProviderFactory) New(context.Context, resource.Input) (any, error) {
	return inference.ProviderDefinition{ID: f.name}, nil
}

func TestAssemblyCollectsProviderDeps(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(fakeProviderFactory{name: "openai"})
	reg.MustRegister(fakeProviderFactory{name: "qwen"})
	if err := inference.Register(reg); err != nil {
		t.Fatalf("inference.Register: %v", err)
	}

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"openai_provider": {Kind: "inference.Provider", Impl: "openai"},
			"qwen_provider":   {Kind: "inference.Provider", Impl: "qwen"},
			"infer": {
				Kind: "inference.Assembly", Impl: "unified",
				Deps: resource.Deps{
					"provider.openai": "openai_provider",
					"provider.qwen":   "qwen_provider",
				},
			},
		},
	}
	result, err := deploy.NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()

	value, ok := result.Value("infer")
	if !ok {
		t.Fatal("assembly missing from result")
	}
	assembly := value.(*inference.Assembly)
	providers := assembly.Providers()
	if len(providers) != 2 ||
		providers[0].ID != "openai" || providers[1].ID != "qwen" {
		t.Fatalf("providers = %+v, want [openai qwen]", providers)
	}
}

func TestAssemblyRejectsMissingProviders(t *testing.T) {
	reg := resource.NewRegistry()
	if err := inference.Register(reg); err != nil {
		t.Fatalf("inference.Register: %v", err)
	}
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"infer": {Kind: "inference.Assembly", Impl: "unified"},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); err == nil {
		t.Fatal("Build unexpectedly accepted assembly without providers")
	}
}
