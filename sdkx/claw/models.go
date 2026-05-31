package claw

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/anthropic"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/mock"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/ollama"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

// ModelsConfig defines named LLM and embedding clients.
type ModelsConfig struct {
	Chat      string                 `json:"chat,omitempty" yaml:"chat,omitempty"`
	Extractor string                 `json:"extractor,omitempty" yaml:"extractor,omitempty"`
	Embedder  string                 `json:"embedder,omitempty" yaml:"embedder,omitempty"`
	LLM       map[string]ModelConfig `json:"llm,omitempty" yaml:"llm,omitempty"`
	Embedding map[string]ModelConfig `json:"embedding,omitempty" yaml:"embedding,omitempty"`
	// Embeddings is accepted as a plural spelling for config readability.
	Embeddings map[string]ModelConfig `json:"embeddings,omitempty" yaml:"embeddings,omitempty"`
}

// ModelConfig is forwarded to provider registries after credential expansion.
type ModelConfig struct {
	Provider   string         `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model      string         `json:"model,omitempty" yaml:"model,omitempty"`
	APIKey     string         `json:"api_key,omitempty" yaml:"api_key,omitempty"`
	APIKeyEnv  string         `json:"api_key_env,omitempty" yaml:"api_key_env,omitempty"`
	BaseURL    string         `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	APIVersion string         `json:"api_version,omitempty" yaml:"api_version,omitempty"`
	Region     string         `json:"region,omitempty" yaml:"region,omitempty"`
	Config     map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

func (c *Claw) chatModel(ctx context.Context) (llm.LLM, error) {
	return c.model(ctx, c.cfg.Models.Chat)
}

func (c *Claw) extractorModel(ctx context.Context) (llm.LLM, bool, error) {
	name := strings.TrimSpace(c.cfg.Models.Extractor)
	if name == "" {
		return nil, false, nil
	}
	client, err := c.model(ctx, name)
	if err != nil {
		return nil, false, err
	}
	return client, true, nil
}

func (c *Claw) embedder(ctx context.Context) (embedding.Embedder, bool, error) {
	name := strings.TrimSpace(c.cfg.Models.Embedder)
	if name == "" {
		return nil, false, nil
	}
	emb, err := c.embedderByName(ctx, name)
	if err != nil {
		return nil, false, err
	}
	return emb, true, nil
}

func (c *Claw) embedderByName(_ context.Context, name string) (embedding.Embedder, error) {
	cfg, ok := c.embeddingConfigs()[strings.TrimSpace(name)]
	if !ok {
		return nil, fmt.Errorf("claw: embedding model %q is not configured", name)
	}
	cfg.applyCredentialEnv()
	return embedding.NewFromConfig(cfg.Provider, cfg.Model, cfg.providerConfig())
}

func (c *Claw) embeddingConfigs() map[string]ModelConfig {
	if len(c.cfg.Models.Embeddings) == 0 {
		return c.cfg.Models.Embedding
	}
	if len(c.cfg.Models.Embedding) == 0 {
		return c.cfg.Models.Embeddings
	}
	out := make(map[string]ModelConfig, len(c.cfg.Models.Embedding)+len(c.cfg.Models.Embeddings))
	for k, v := range c.cfg.Models.Embedding {
		out[k] = v
	}
	for k, v := range c.cfg.Models.Embeddings {
		out[k] = v
	}
	return out
}

func (c *Claw) model(ctx context.Context, name string) (llm.LLM, error) {
	name = strings.TrimSpace(name)
	if strings.Contains(name, "/") && c.resolver != nil {
		return c.resolver.Resolve(ctx, name)
	}
	if name == "" {
		return nil, fmt.Errorf("claw: model name is empty")
	}
	cfg, ok := c.cfg.Models.LLM[name]
	if !ok {
		return nil, fmt.Errorf("claw: llm model %q is not configured", name)
	}
	cfg.applyCredentialEnv()
	return llm.NewFromConfig(cfg.Provider, cfg.Model, cfg.providerConfig())
}

func (c *Claw) buildResolver() llm.LLMResolver {
	if c.chat != nil {
		return fixedResolver{client: c.chat}
	}
	return llm.DefaultResolver(modelStore{models: c.cfg.Models}, llm.WithFallbackModel(c.cfg.modelRef(c.cfg.Models.Chat)))
}

func (c Config) modelRef(name string) string {
	cfg, ok := c.Models.LLM[strings.TrimSpace(name)]
	if !ok || cfg.Provider == "" || cfg.Model == "" {
		return name
	}
	return cfg.Provider + "/" + cfg.Model
}

func (m *ModelConfig) applyCredentialEnv() {
	if m.APIKey == "" && m.APIKeyEnv != "" {
		m.APIKey = os.Getenv(m.APIKeyEnv)
	}
}

func (m ModelConfig) providerConfig() map[string]any {
	out := make(map[string]any, len(m.Config)+4)
	for k, v := range m.Config {
		out[k] = v
	}
	if m.APIKey != "" {
		out["api_key"] = m.APIKey
	}
	if m.BaseURL != "" {
		out["base_url"] = m.BaseURL
	}
	if m.APIVersion != "" {
		out["api_version"] = m.APIVersion
	}
	if m.Region != "" {
		out["region"] = m.Region
	}
	return out
}

type fixedResolver struct {
	client llm.LLM
}

func (r fixedResolver) Resolve(context.Context, string) (llm.LLM, error) {
	if r.client == nil {
		return nil, errdefs.NotFoundf("claw: fixed llm resolver has no client")
	}
	return r.client, nil
}

func (r fixedResolver) InvalidateCache(...llm.InvalidateOption) {}

type modelStore struct {
	models ModelsConfig
}

func (s modelStore) GetProviderConfig(_ context.Context, provider, _ string) (*llm.ProviderConfig, error) {
	for _, cfg := range s.models.LLM {
		if cfg.Provider != provider {
			continue
		}
		cfg.applyCredentialEnv()
		return &llm.ProviderConfig{
			Provider: provider,
			Config:   cfg.providerConfig(),
		}, nil
	}
	return nil, errdefs.NotFoundf("claw: provider %q is not configured", provider)
}

func (s modelStore) GetModelConfig(_ context.Context, provider, modelName string) (*llm.ModelConfig, error) {
	for _, cfg := range s.models.LLM {
		if cfg.Provider == provider && cfg.Model == modelName {
			cfg.applyCredentialEnv()
			return &llm.ModelConfig{
				Provider: provider,
				Model:    modelName,
				Extra:    cfg.providerConfig(),
			}, nil
		}
	}
	return nil, errdefs.NotFoundf("claw: model %s/%s is not configured", provider, modelName)
}
