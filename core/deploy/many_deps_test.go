package deploy_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

type providerValue struct{ name string }

type providerFactory struct{ name string }

func (f providerFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "inference.Provider", Impl: f.name}
}

func (f providerFactory) New(context.Context, resource.Input) (any, error) {
	return providerValue{name: f.name}, nil
}

type assemblyFactory struct{ got *[]any }

func (assemblyFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "inference.Assembly", Impl: "unified",
		Deps: []resource.DepSpec{{
			Name: "provider", Type: "inference.Provider", Required: true, Many: true,
		}},
	}
}

func (f assemblyFactory) New(_ context.Context, in resource.Input) (any, error) {
	*f.got = in.DepsMany("provider")
	return "assembly", nil
}

func TestBuildManyDeps(t *testing.T) {
	var got []any
	reg := resource.NewRegistry()
	reg.MustRegister(providerFactory{name: "openai"})
	reg.MustRegister(providerFactory{name: "qwen"})
	reg.MustRegister(assemblyFactory{got: &got})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"openai_provider": {Kind: "inference.Provider", Impl: "openai"},
			"qwen_provider":   {Kind: "inference.Provider", Impl: "qwen"},
			"infer": {
				Kind: "inference.Assembly", Impl: "unified",
				Deps: resource.Deps{
					"provider.qwen":   "qwen_provider",
					"provider.openai": "openai_provider",
				},
			},
		},
	}
	result, err := deploy.NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()
	if len(got) != 2 {
		t.Fatalf("DepsMany = %v, want 2 providers", got)
	}
	if got[0].(providerValue).name != "openai" || got[1].(providerValue).name != "qwen" {
		t.Fatalf("DepsMany order = %v, want [openai qwen]", got)
	}
}

func TestBuildRejectsUndeclaredDep(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(providerFactory{name: "openai"})
	reg.MustRegister(assemblyFactory{})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"openai_provider": {Kind: "inference.Provider", Impl: "openai"},
			"infer": {
				Kind: "inference.Assembly", Impl: "unified",
				Deps: resource.Deps{
					"provider.openai": "openai_provider",
					"bogus":           "openai_provider",
				},
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation for undeclared dep", err)
	}
}

func TestBuildRejectsMissingRequiredManyDep(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(assemblyFactory{})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"infer": {Kind: "inference.Assembly", Impl: "unified"},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation for missing many dep", err)
	}
}

func TestBuildRejectsMissingRequiredFixedDep(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(sandboxFactory{})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"box": {Kind: "sandbox.Registry", Impl: "local"},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation for missing fixed dep", err)
	}
}
