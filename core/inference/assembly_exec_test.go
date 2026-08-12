package inference

import (
	"context"
	"testing"
)

type stubGenerateDriver struct {
	resp GenerateResponse
}

func (stubGenerateDriver) inferenceGenerateDriver() {}

func (stubGenerateDriver) Explain(
	context.Context, ModelRef, GenerateRequest,
) (Explanation, error) {
	return Explanation{}, nil
}

func (d stubGenerateDriver) Execute(
	context.Context, ModelRef, GenerateRequest,
) (GenerateResponse, error) {
	return d.resp, nil
}

type stubEmbedDriver struct {
	resp EmbedResponse
}

func (stubEmbedDriver) inferenceEmbedDriver() {}

func (stubEmbedDriver) Explain(
	context.Context, ModelRef, EmbedRequest,
) (Explanation, error) {
	return Explanation{}, nil
}

func (d stubEmbedDriver) Execute(
	context.Context, ModelRef, EmbedRequest,
) (EmbedResponse, error) {
	return d.resp, nil
}

func testProvider() ProviderDefinition {
	return ProviderDefinition{
		ID: "fake",
		Models: []ModelImplementation{{
			Descriptor: ModelDescriptor{
				ID: ModelID{Provider: "fake", Name: "model-1"},
			},
			Openers: Openers{
				Generate: func(
					context.Context, ModelRef,
				) (GenerateOperations, error) {
					return GenerateOperations{Unary: stubGenerateDriver{
						resp: GenerateResponse{Usage: Usage{TotalTokens: 7}},
					}}, nil
				},
				Embed: func(
					context.Context, ModelRef,
				) (EmbedDriver, error) {
					return stubEmbedDriver{
						resp: EmbedResponse{Usage: EmbedUsage{ItemCount: 3}},
					}, nil
				},
			},
		}},
	}
}

func testAssembly() *Assembly {
	return &Assembly{providers: map[string]ProviderDefinition{
		"fake": testProvider(),
	}}
}

func TestAssemblyGenerate(t *testing.T) {
	response, err := testAssembly().Generate(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
		GenerateRequest{},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want TotalTokens 7", response.Usage)
	}
}

func TestAssemblyEmbed(t *testing.T) {
	response, err := testAssembly().Embed(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
		EmbedRequest{},
	)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if response.Usage.ItemCount != 3 {
		t.Fatalf("usage = %+v, want ItemCount 3", response.Usage)
	}
}

func TestAssemblyUnknownModel(t *testing.T) {
	_, err := testAssembly().Generate(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "nope"}},
		GenerateRequest{},
	)
	if !IsKind(err, UnknownModel) {
		t.Fatalf("Generate error = %v, want UnknownModel", err)
	}
}

func TestAssemblyUnknownProvider(t *testing.T) {
	_, err := testAssembly().Generate(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "nope", Name: "model-1"}},
		GenerateRequest{},
	)
	if !IsKind(err, UnknownProvider) {
		t.Fatalf("Generate error = %v, want UnknownProvider", err)
	}
}

func TestAssemblyProfileDenied(t *testing.T) {
	provider := testProvider()
	provider.Profiles = []ProfileDefinition{{
		ID:         "embed-only",
		Operations: []Operation{OperationEmbed},
	}}
	assembly := &Assembly{providers: map[string]ProviderDefinition{
		"fake": provider,
	}}
	_, err := assembly.Generate(
		context.Background(),
		ModelRef{
			ID:      ModelID{Provider: "fake", Name: "model-1"},
			Profile: "embed-only",
		},
		GenerateRequest{},
	)
	if !IsKind(err, UnsupportedOperation) {
		t.Fatalf("Generate error = %v, want UnsupportedOperation", err)
	}
}

func TestAssemblyModels(t *testing.T) {
	models := testAssembly().Models()
	if len(models) != 1 ||
		models[0].ID.Provider != "fake" || models[0].ID.Name != "model-1" {
		t.Fatalf("Models = %+v", models)
	}
	if _, err := testAssembly().InspectModel(
		ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
	); err != nil {
		t.Fatalf("InspectModel failed for known model: %v", err)
	}
}
