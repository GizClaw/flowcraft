package claw

import (
	"context"
	"testing"
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
