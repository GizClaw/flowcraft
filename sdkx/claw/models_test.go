package claw

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

func TestModelAliasResolverResolvesNamedConfig(t *testing.T) {
	cfg := Config{
		Models: ModelsConfig{
			Chat: "fast",
			LLM: map[string]ModelConfig{
				"fast": {Provider: "mock", Model: "mock-fast"},
			},
		},
	}
	r := (&Claw{cfg: cfg}).buildResolver()
	if _, err := r.Resolve(context.Background(), "fast"); err != nil {
		t.Fatalf("Resolve named model: %v", err)
	}
}

func TestModelStoreDoesNotLeakBaseURLAcrossSameProvider(t *testing.T) {
	provider := fmt.Sprintf("clawmodeltest%d", testProviderSeq.Add(1))
	var gotConfig map[string]any
	llm.RegisterProvider(provider, func(_ string, config map[string]any) (llm.LLM, error) {
		gotConfig = config
		return staticLLM{reply: "ok"}, nil
	})

	cfg := Config{
		Models: ModelsConfig{
			Chat: "direct",
			LLM: map[string]ModelConfig{
				"direct": {Provider: provider, Model: "same-model", APIKey: "direct-key"},
				"proxy":  {Provider: provider, Model: "proxy-model", APIKey: "proxy-key", BaseURL: "https://proxy.example/v1"},
			},
		},
	}
	r := (&Claw{cfg: cfg}).buildResolver()
	if _, err := r.Resolve(context.Background(), "direct"); err != nil {
		t.Fatalf("Resolve direct model: %v", err)
	}
	if got := gotConfig["api_key"]; got != "direct-key" {
		t.Fatalf("api_key = %v, want direct-key", got)
	}
	if got := gotConfig["base_url"]; got != nil {
		t.Fatalf("base_url leaked across same provider: %v", got)
	}
}
