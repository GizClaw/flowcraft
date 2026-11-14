package inference_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

type stubGenerator struct{}

func (stubGenerator) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, nil
}
func (stubGenerator) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return nil, nil
}

type stubCatalog struct{ models []model.ModelDescriptor }

func (c stubCatalog) Models() []model.ModelDescriptor { return c.models }
func (c stubCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range c.models {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}

// A line-up that declares a capability the provider cannot serve is a
// deployment error, not something a caller discovers at the first request.
func TestProviderValidateCatchesUncallableDeclarations(t *testing.T) {
	embedding := stubCatalog{models: []model.ModelDescriptor{{
		ID:           model.ModelID{Provider: "p", Name: "embed"},
		Capabilities: model.ModelCapabilities{EmbeddingDimensions: 1536},
	}}}
	provider := inference.Provider{Generator: stubGenerator{}, Catalog: embedding}
	if err := provider.Validate(); err == nil {
		t.Error("a declared embedding width with no embedder was accepted")
	}

	declaring := stubCatalog{models: []model.ModelDescriptor{{
		ID:           model.ModelID{Provider: "p", Name: "judge"},
		Capabilities: model.ModelCapabilities{Decision: true},
	}}}
	provider = inference.Provider{Generator: stubGenerator{}, Catalog: declaring}
	if err := provider.Validate(); err == nil {
		t.Error("declared decision support with no decider was accepted")
	}

	quiet := stubCatalog{models: []model.ModelDescriptor{{
		ID: model.ModelID{Provider: "p", Name: "chat"},
	}}}
	if err := (inference.Provider{Generator: stubGenerator{}, Catalog: quiet}).Validate(); err != nil {
		t.Errorf("a plain line-up was rejected: %v", err)
	}
	if err := (inference.Provider{}).Validate(); err == nil {
		t.Error("a provider with no generator was accepted")
	}
}
