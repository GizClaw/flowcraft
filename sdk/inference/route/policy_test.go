package route

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

func TestPolicySupportsOnlyCurrentOperations(t *testing.T) {
	model := inference.ModelRef{
		ID: inference.ModelID{Provider: "fake", Name: "multimodal"},
	}
	pool := func(tier Tier) []Pool {
		return []Pool{{Tier: tier, Targets: []Target{{Model: model}}}}
	}
	policy := Policy{
		Generate:      pool("quality"),
		Embed:         pool("search"),
		Transcription: pool("audio"),
		Realtime:      pool("live"),
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	clone := policy.Clone()
	clone.Generate[0].Tier = "changed"
	if policy.Generate[0].Tier != "quality" {
		t.Fatal("policy clone shares generate pools")
	}

	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "fake",
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{ID: model.ID},
			Openers: inference.Openers{
				Generate: func(context.Context, inference.ModelRef) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{}, nil
				},
				Embed: func(context.Context, inference.ModelRef) (inference.EmbedDriver, error) {
					return nil, nil
				},
				Transcription: func(context.Context, inference.ModelRef) (inference.TranscriptionOperations, error) {
					return inference.TranscriptionOperations{}, nil
				},
				Realtime: func(context.Context, inference.ModelRef) (inference.RealtimeDriver, error) {
					return nil, nil
				},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if err := policy.ValidateFor(runtime); err != nil {
		t.Fatalf("ValidateFor: %v", err)
	}
}
