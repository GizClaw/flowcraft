package anthropic_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference-anthropic"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func modelDescriptor(name string) model.ModelDescriptor {
	return model.ModelDescriptor{
		ID: model.ModelID{Provider: anthropic.ProviderID, Name: name},
		Capabilities: model.ModelCapabilities{
			Outputs: []message.PartKind{message.PartText},
		},
	}
}

func TestSettingsValidate(t *testing.T) {
	if err := (anthropic.Settings{APIKey: "key"}).Validate(); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	for name, settings := range map[string]anthropic.Settings{
		"missing key":      {},
		"negative retry":   {APIKey: "key", Retries: -1},
		"negative timeout": {APIKey: "key", Timeout: -time.Second},
	} {
		if err := settings.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, settings)
		}
	}
}

func TestSettingsRejectsForeignModels(t *testing.T) {
	descriptor := modelDescriptor("claude")
	descriptor.ID.Provider = "openai"
	if err := (anthropic.Settings{
		APIKey: "key",
		Models: []model.ModelDescriptor{descriptor},
	}).Validate(); err == nil {
		t.Fatal("Validate accepted a model from another provider")
	}
}
