package claw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// LoadConfig reads Claw configuration from the workspace without expanding
// environment variables. Call Config.ExpandEnv explicitly at process edges
// such as CLIs that want ${VAR} substitution.
func LoadConfig(ctx context.Context, ws sdkworkspace.Workspace) (Config, error) {
	return loadConfig(ctx, ws)
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

// ExpandEnv expands ${VAR} and $VAR in every string field in Config.
func (c *Config) ExpandEnv() {
	if c == nil {
		return
	}
	expandEnvValue(reflect.ValueOf(c).Elem())
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

func expandEnvValue(v reflect.Value) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		expandEnvValue(v.Elem())
		return
	}
	if !v.CanSet() && v.Kind() != reflect.Map && v.Kind() != reflect.Slice && v.Kind() != reflect.Struct && v.Kind() != reflect.Interface {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(os.ExpandEnv(v.String()))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if field.CanSet() || field.Kind() == reflect.Map || field.Kind() == reflect.Slice || field.Kind() == reflect.Struct || field.Kind() == reflect.Pointer || field.Kind() == reflect.Interface {
				expandEnvValue(field)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			expandEnvValue(v.Index(i))
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}
		for _, key := range v.MapKeys() {
			value := expandEnvMapValue(v.MapIndex(key))
			v.SetMapIndex(key, value)
		}
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		expanded := expandEnvInterface(v.Interface())
		if v.CanSet() {
			v.Set(reflect.ValueOf(expanded))
		}
	}
}

func expandEnvMapValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	if v.Kind() == reflect.Struct || v.Kind() == reflect.Map || v.Kind() == reflect.Slice || v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		cp := reflect.New(v.Type()).Elem()
		cp.Set(v)
		expandEnvValue(cp)
		return cp
	}
	expanded := expandEnvInterface(v.Interface())
	if expanded == nil {
		return reflect.Zero(v.Type())
	}
	ev := reflect.ValueOf(expanded)
	if ev.Type().AssignableTo(v.Type()) {
		return ev
	}
	if ev.Type().ConvertibleTo(v.Type()) {
		return ev.Convert(v.Type())
	}
	return v
}

func expandEnvInterface(v any) any {
	switch x := v.(type) {
	case string:
		return os.ExpandEnv(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = expandEnvInterface(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = expandEnvInterface(item)
		}
		return out
	default:
		return v
	}
}
