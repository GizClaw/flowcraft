package claw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	"gopkg.in/yaml.v3"
)

const configDir = "config"

// Config describes a self-contained local Claw runtime.
type Config struct {
	Workspace WorkspaceConfig `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	Models    ModelsConfig    `json:"models,omitempty" yaml:"models,omitempty"`
	Memory    MemoryConfig    `json:"memory,omitempty" yaml:"memory,omitempty"`
	Agent     AgentConfig     `json:"agent,omitempty" yaml:"agent,omitempty"`
}

// WorkspaceConfig names the workspace subtrees Claw owns.
type WorkspaceConfig struct {
	RecallRoot    string `json:"recall_root,omitempty" yaml:"recall_root,omitempty"`
	RetrievalRoot string `json:"retrieval_root,omitempty" yaml:"retrieval_root,omitempty"`
}

type localRoot interface {
	Root() string
}

func defaultConfig() Config {
	return Config{
		Workspace: WorkspaceConfig{
			RecallRoot:    "memory/recall",
			RetrievalRoot: "memory/retrieval/bbh",
		},
		Models: ModelsConfig{
			Chat: "default",
			LLM: map[string]ModelConfig{
				"default": {
					Provider: "mock",
					Model:    "mock-default",
				},
			},
		},
		Memory: MemoryConfig{
			Enabled:          false,
			Backend:          "memory",
			RuntimeID:        "claw",
			UserID:           "local",
			TopK:             5,
			SaveConversation: true,
		},
		Agent: AgentConfig{
			ID:            "claw",
			Name:          "Claw",
			MaxIterations: 1,
		},
	}
}

func loadConfig(ctx context.Context, ws sdkworkspace.Workspace) (Config, error) {
	cfg := defaultConfig()
	if ws == nil {
		return cfg, nil
	}

	if ok, err := mergeConfigFile(ctx, ws, "config/claw.yaml", &cfg); err != nil {
		return Config{}, err
	} else if ok {
		cfg.applyDefaults()
		return cfg, nil
	}
	if ok, err := mergeConfigFile(ctx, ws, "config/claw.yml", &cfg); err != nil {
		return Config{}, err
	} else if ok {
		cfg.applyDefaults()
		return cfg, nil
	}
	if ok, err := mergeConfigFile(ctx, ws, "config/claw.json", &cfg); err != nil {
		return Config{}, err
	} else if ok {
		cfg.applyDefaults()
		return cfg, nil
	}

	if _, err := mergeConfigFile(ctx, ws, "config/workspace.yaml", &cfg.Workspace); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/workspace.yml", &cfg.Workspace); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/workspace.json", &cfg.Workspace); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/models.yaml", &cfg.Models); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/models.yml", &cfg.Models); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/models.json", &cfg.Models); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/memory.yaml", &cfg.Memory); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/memory.yml", &cfg.Memory); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/memory.json", &cfg.Memory); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/agent.yaml", &cfg.Agent); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/agent.yml", &cfg.Agent); err != nil {
		return Config{}, err
	}
	if _, err := mergeConfigFile(ctx, ws, "config/agent.json", &cfg.Agent); err != nil {
		return Config{}, err
	}

	cfg.applyDefaults()
	return cfg, nil
}

func mergeConfigFile(ctx context.Context, ws sdkworkspace.Workspace, path string, out any) (bool, error) {
	raw, err := ws.Read(ctx, path)
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if strings.HasSuffix(path, ".json") {
		if err := json.Unmarshal(raw, out); err != nil {
			return false, fmt.Errorf("claw: decode %s: %w", path, err)
		}
		return true, nil
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("claw: decode %s: %w", path, err)
	}
	return true, nil
}

func (c *Config) applyDefaults() {
	def := defaultConfig()
	if strings.TrimSpace(c.Workspace.RecallRoot) == "" {
		c.Workspace.RecallRoot = def.Workspace.RecallRoot
	}
	if strings.TrimSpace(c.Workspace.RetrievalRoot) == "" {
		c.Workspace.RetrievalRoot = def.Workspace.RetrievalRoot
	}
	if strings.TrimSpace(c.Models.Chat) == "" {
		c.Models.Chat = def.Models.Chat
	}
	if c.Models.LLM == nil {
		c.Models.LLM = map[string]ModelConfig{}
	}
	if _, ok := c.Models.LLM[c.Models.Chat]; !ok && c.Models.Chat == def.Models.Chat {
		c.Models.LLM[def.Models.Chat] = def.Models.LLM[def.Models.Chat]
	}
	if strings.TrimSpace(c.Memory.Backend) == "" {
		c.Memory.Backend = def.Memory.Backend
	}
	if strings.TrimSpace(c.Memory.RuntimeID) == "" {
		c.Memory.RuntimeID = def.Memory.RuntimeID
	}
	if strings.TrimSpace(c.Memory.UserID) == "" {
		c.Memory.UserID = def.Memory.UserID
	}
	if c.Memory.TopK <= 0 {
		c.Memory.TopK = def.Memory.TopK
	}
	if strings.TrimSpace(c.Agent.ID) == "" {
		c.Agent.ID = def.Agent.ID
	}
	if strings.TrimSpace(c.Agent.Name) == "" {
		c.Agent.Name = def.Agent.Name
	}
	if c.Agent.MaxIterations <= 0 {
		c.Agent.MaxIterations = def.Agent.MaxIterations
	}
}

func localSubWorkspace(ws sdkworkspace.Workspace, rel string) (sdkworkspace.Workspace, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return ws, nil
	}
	rooted, ok := ws.(localRoot)
	if !ok {
		return nil, fmt.Errorf("claw: workspace subtree %q requires a local workspace", rel)
	}
	return sdkworkspace.NewLocalWorkspace(filepath.Join(rooted.Root(), filepath.FromSlash(rel)))
}
