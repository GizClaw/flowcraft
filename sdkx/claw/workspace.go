package claw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

const defaultConfigRoot = "config"

// Config describes a self-contained local Claw runtime.
type Config struct {
	Workspace WorkspaceConfig `json:"workspace,omitempty"`
	Models    ModelsConfig    `json:"models,omitempty"`
	Memory    MemoryConfig    `json:"memory,omitempty"`
	Agent     AgentConfig     `json:"agent,omitempty"`
}

// WorkspaceConfig names the workspace subtrees Claw owns.
type WorkspaceConfig struct {
	MemoryRoot string `json:"memory_root,omitempty"`
	StateRoot  string `json:"state_root,omitempty"`
}

type configOverrides struct {
	workspace *WorkspaceConfig
	models    *ModelsConfig
	memory    *MemoryConfig
	agent     *AgentConfig
}

// DefaultConfig returns Claw's in-process defaults without reading
// workspace configuration files.
func DefaultConfig() Config {
	return defaultConfig()
}

func defaultConfig() Config {
	return Config{
		Workspace: WorkspaceConfig{
			MemoryRoot: "memory",
			StateRoot:  "state",
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
			Enabled: false,
			Scope: MemoryScopeConfig{
				RuntimeID: "claw",
				UserID:    "local",
			},
			Write: MemoryWriteConfig{
				SaveConversation: true,
				Mode:             "sync",
				Tier:             "general",
			},
			Recall: MemoryRecallConfig{
				Enabled: true,
				TopK:    5,
			},
			Retrieval: MemoryRetrievalConfig{
				Backend: "bbh",
			},
		},
		Agent: AgentConfig{
			ID:            "claw",
			Name:          "Claw",
			MaxIterations: 8,
		},
	}
}

func loadConfig(ctx context.Context, ws sdkworkspace.Workspace, root string) (Config, error) {
	cfg := defaultConfig()
	if ws == nil {
		cfg.applyDefaults()
		return cfg, nil
	}
	root = cleanConfigRoot(root)
	if _, err := mergeJSONConfigFile(ctx, ws, joinConfigPath(root, "workspace.json"), &cfg.Workspace); err != nil {
		return Config{}, err
	}
	if _, err := mergeJSONConfigFile(ctx, ws, joinConfigPath(root, "models.json"), &cfg.Models); err != nil {
		return Config{}, err
	}
	if _, err := mergeJSONConfigFile(ctx, ws, joinConfigPath(root, "memory.json"), &cfg.Memory); err != nil {
		return Config{}, err
	}
	if _, err := mergeJSONConfigFile(ctx, ws, joinConfigPath(root, "agent.json"), &cfg.Agent); err != nil {
		return Config{}, err
	}
	cfg.applyDefaults()
	return cfg, nil
}

func mergeJSONConfigFile(ctx context.Context, ws sdkworkspace.Workspace, path string, out any) (bool, error) {
	raw, err := ws.Read(ctx, path)
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("claw: decode %s: %w", path, err)
	}
	return true, nil
}

func cleanConfigRoot(root string) string {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		return defaultConfigRoot
	}
	if root == "." {
		return "."
	}
	return root
}

func joinConfigPath(root, name string) string {
	if root == "" || root == "." {
		return name
	}
	return root + "/" + name
}

func (c *Config) applyOverrides(overrides configOverrides) {
	if overrides.workspace != nil {
		c.Workspace = *overrides.workspace
	}
	if overrides.models != nil {
		c.Models = *overrides.models
	}
	if overrides.memory != nil {
		c.Memory = *overrides.memory
	}
	if overrides.agent != nil {
		c.Agent = *overrides.agent
	}
}

func (c *Config) applyDefaults() {
	def := defaultConfig()
	if strings.TrimSpace(c.Workspace.MemoryRoot) == "" {
		c.Workspace.MemoryRoot = def.Workspace.MemoryRoot
	}
	if strings.TrimSpace(c.Workspace.StateRoot) == "" {
		c.Workspace.StateRoot = def.Workspace.StateRoot
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
		c.Memory.Backend = def.Memory.Retrieval.Backend
	}
	if strings.TrimSpace(c.Memory.Scope.RuntimeID) == "" {
		c.Memory.Scope.RuntimeID = firstNonEmpty(c.Memory.RuntimeID, def.Memory.Scope.RuntimeID)
	}
	if strings.TrimSpace(c.Memory.Scope.UserID) == "" {
		c.Memory.Scope.UserID = firstNonEmpty(c.Memory.UserID, def.Memory.Scope.UserID)
	}
	if strings.TrimSpace(c.Memory.Scope.AgentID) == "" {
		c.Memory.Scope.AgentID = firstNonEmpty(c.Memory.AgentID, c.Agent.ID)
	}
	if strings.TrimSpace(c.Memory.Retrieval.Backend) == "" {
		c.Memory.Retrieval.Backend = firstNonEmpty(c.Memory.Backend, def.Memory.Retrieval.Backend)
	}
	if c.Memory.Recall.TopK <= 0 {
		if c.Memory.TopK > 0 {
			c.Memory.Recall.TopK = c.Memory.TopK
		} else {
			c.Memory.Recall.TopK = def.Memory.Recall.TopK
		}
	}
	if c.Memory.SaveConversation {
		c.Memory.Write.SaveConversation = true
	}
	if c.Memory.Write.Mode == "" {
		c.Memory.Write.Mode = def.Memory.Write.Mode
	}
	if c.Memory.Write.Tier == "" {
		c.Memory.Write.Tier = def.Memory.Write.Tier
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
	c.ensureAgentGraph()
}
