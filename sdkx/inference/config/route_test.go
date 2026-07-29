package config

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
)

func routeTarget(provider, name string, quality float64) route.Target {
	return route.Target{
		Model: inference.ModelRef{
			ID: inference.ModelID{Provider: provider, Name: name},
		},
		Score: route.ModelScore{Quality: &quality},
	}
}

func TestDocumentValidatesAndClonesRouteSection(t *testing.T) {
	quality := 0.9
	document := Document{
		Version: VersionV1,
		Providers: []ProviderConfig{{
			ID: "openai", Driver: "openai",
		}},
		Route: &route.Policy{
			Generate: []route.Pool{{
				Tier:    "quality",
				Targets: []route.Target{routeTarget("openai", "gpt", quality)},
			}},
		},
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	clone := document.Clone()
	clone.Route.Generate[0].Tier = "changed"
	*clone.Route.Generate[0].Targets[0].Score.Quality = 0.1
	if document.Route.Generate[0].Tier != "quality" {
		t.Fatal("document clone shares route pools")
	}
	if *document.Route.Generate[0].Targets[0].Score.Quality != 0.9 {
		t.Fatal("document clone shares route scores")
	}

	invalid := document.Clone()
	invalid.Route.Generate = append(
		invalid.Route.Generate,
		route.Pool{Tier: "quality", Targets: []route.Target{
			routeTarget("openai", "gpt", quality),
		}},
	)
	if err := invalid.Validate(); err == nil ||
		!strings.Contains(err.Error(), "duplicate tier") {
		t.Fatalf("duplicate tier error = %v", err)
	}
}

func TestDecodeJSONRoundTripsRouteSection(t *testing.T) {
	document, err := DecodeJSON(strings.NewReader(`{
		"version": "v1",
		"providers": [{"id": "openai", "driver": "openai"}],
		"route": {
			"generate": [{
				"tier": "balanced",
				"targets": [
					{"model": {"id": {"provider": "openai", "name": "gpt"}}, "score": {"speed": 0.8}},
					{"model": {"id": {"provider": "openai", "name": "gpt-mini"}, "profile": "eu"}}
				]
			}]
		}
	}`))
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if document.Route == nil ||
		len(document.Route.Generate) != 1 ||
		len(document.Route.Generate[0].Targets) != 2 {
		t.Fatalf("route = %+v", document.Route)
	}
	first := document.Route.Generate[0].Targets[0]
	if first.Score.Speed == nil || *first.Score.Speed != 0.8 {
		t.Fatalf("score = %+v", first.Score)
	}
	second := document.Route.Generate[0].Targets[1]
	if second.Model.Profile != "eu" {
		t.Fatalf("profile = %q", second.Model.Profile)
	}
}

func TestBuilderValidatesRouteTargetsAgainstRuntime(t *testing.T) {
	builder, err := NewBuilder(
		map[string]Factory{
			"fake": FactoryFunc(func(
				_ context.Context,
				input ProviderInput,
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
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	document := Document{
		Version:   VersionV1,
		Providers: []ProviderConfig{{ID: "fake", Driver: "fake"}},
		Route: &route.Policy{
			Generate: []route.Pool{{
				Tier:    "quality",
				Targets: []route.Target{routeTarget("fake", "ghost", 0.5)},
			}},
		},
	}
	if _, err := builder.NewRuntime(t.Context(), document); err == nil ||
		!strings.Contains(err.Error(), "route") {
		t.Fatalf("unknown route target error = %v", err)
	}

	document.Route.Generate[0].Targets[0] = routeTarget("fake", "model", 0.5)
	if _, err := builder.NewRuntime(t.Context(), document); err != nil {
		t.Fatalf("NewRuntime with valid route: %v", err)
	}
}

func newGenerateFactory(t *testing.T) Factory {
	t.Helper()
	driver, err := inference.BindGenerate(
		func(
			_ context.Context,
			_ inference.ModelRef,
			request inference.GenerateRequest,
			shape inference.GenerateExecutionShape,
		) (inference.Compiled[string], error) {
			active := request.ActiveFieldsFor(shape)
			decisions := make([]inference.Decision, len(active))
			for index, field := range active {
				decisions[index] = inference.Decision{
					Field: field, Disposition: inference.Native,
				}
			}
			return inference.Compiled[string]{
				Wire: "wire",
				Report: inference.CompileReport{
					Operation: inference.OperationGenerate, Decisions: decisions,
				},
			}, nil
		},
		func(context.Context, string) (string, error) { return "ok", nil },
		func(context.Context, string) (inference.GenerateResponse, error) {
			return inference.GenerateResponse{
				Message: inference.Message{
					Role: inference.RoleAssistant,
					Content: inference.Content{
						Parts: []inference.Part{inference.TextPart{Text: "ok"}},
					},
				},
				FinishReason: inference.FinishCompleted,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	return FactoryFunc(func(
		_ context.Context,
		input ProviderInput,
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
						return inference.GenerateOperations{Unary: driver}, nil
					},
				},
			}},
		}, nil
	})
}

func TestBuilderNewAssembly(t *testing.T) {
	builder, err := NewBuilder(
		map[string]Factory{"fake": newGenerateFactory(t)},
		nil,
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	document := Document{
		Version:   VersionV1,
		Providers: []ProviderConfig{{ID: "fake", Driver: "fake"}},
	}

	t.Run("without route section", func(t *testing.T) {
		assembly, err := builder.NewAssembly(t.Context(), document)
		if err != nil {
			t.Fatalf("NewAssembly: %v", err)
		}
		if assembly.Runtime == nil || assembly.Router != nil {
			t.Fatalf("assembly = %+v, want runtime only", assembly)
		}
	})

	t.Run("with route section", func(t *testing.T) {
		routed := document.Clone()
		routed.Route = &route.Policy{
			Generate: []route.Pool{{
				Tier:    "balanced",
				Targets: []route.Target{routeTarget("fake", "model", 0.5)},
			}},
		}
		assembly, err := builder.NewAssembly(t.Context(), routed)
		if err != nil {
			t.Fatalf("NewAssembly: %v", err)
		}
		if assembly.Runtime == nil || assembly.Router == nil {
			t.Fatalf("assembly = %+v, want runtime and router", assembly)
		}
		response, trace, err := assembly.Router.Generate(
			t.Context(),
			inference.GenerateRequest{
				Input: inference.GenerateInput{
					Role: inference.InputRoleUser,
					Content: inference.InputContent{
						Content: inference.Content{
							Parts: []inference.Part{inference.TextPart{Text: "hi"}},
						},
						Intent: inference.Intent{Text: &inference.TextIntent{}},
					},
				},
			},
		)
		if err != nil {
			t.Fatalf("router Generate: %v", err)
		}
		want := inference.ModelRef{
			ID: inference.ModelID{Provider: "fake", Name: "model"},
		}
		if trace.Executed != want || response.Metadata.Model != want.ID {
			t.Fatalf("executed/model = %+v/%+v, want %+v",
				trace.Executed, response.Metadata.Model, want)
		}
	})
}
