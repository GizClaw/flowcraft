package anthropic_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference-anthropic"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

func TestNewPublishesTheCatalog(t *testing.T) {
	provider, err := anthropic.New(anthropic.Settings{
		APIKey: "key",
		Models: []model.ModelDescriptor{modelDescriptor("claude-sonnet")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider.Generator == nil {
		t.Fatal("provider has no generator")
	}
	if provider.Catalog == nil {
		t.Fatal("provider has no catalog")
	}
	if _, ok := provider.Catalog.Describe(model.ModelID{
		Provider: anthropic.ProviderID, Name: "claude-sonnet",
	}); !ok {
		t.Fatal("catalog does not describe the declared model")
	}
}
