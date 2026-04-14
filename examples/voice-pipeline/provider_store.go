package main

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// StaticProviderStore implements llm.ProviderConfigStore with in-memory credentials,
// allowing llm.DefaultResolver to resolve "minimax/<model>" without a server-side store.
type StaticProviderStore struct {
	Provider string
	APIKey   string
	// Default short model name (without "minimax/" prefix); the resolver uses the graph node's model field.
	Model string
}

func (s *StaticProviderStore) GetProviderConfig(_ context.Context, provider string) (*llm.ProviderConfig, error) {
	if provider != s.Provider {
		return nil, errdefs.NotFoundf("provider_config %q not found", provider)
	}
	cfg := map[string]any{"api_key": s.APIKey}
	if s.Model != "" {
		cfg["model"] = s.Model
	}
	return &llm.ProviderConfig{Provider: s.Provider, Config: cfg}, nil
}
