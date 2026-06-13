package claw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	"gopkg.in/yaml.v3"
)

// Config describes a self-contained local Claw runtime.
type Config struct {
	Workspace    WorkspaceConfig     `json:"workspace,omitempty"`
	Conversation ConversationConfig  `json:"conversation,omitempty"`
	Settings     ModelSettingsConfig `json:"settings,omitempty"`
	Models       ModelsConfig        `json:"models,omitempty"`
	History      HistoryConfig       `json:"history,omitempty"`
	Memory       MemoryConfig        `json:"memory,omitempty"`
	Agent        AgentConfig         `json:"agent,omitempty"`
}

// WorkspaceConfig names the workspace subtrees Claw owns.
type WorkspaceConfig struct {
	MemoryRoot  string `json:"memory_root,omitempty"`
	StateRoot   string `json:"state_root,omitempty"`
	HistoryRoot string `json:"history_root,omitempty"`
}

const defaultConversationContextID = "__default__"

// ConversationConfig describes how a Claw participates in a direct
// conversation. Starts is a command/UI hint; SDK request execution still
// treats Text == "" as an ordinary empty-input turn.
type ConversationConfig struct {
	Starts    string `json:"starts,omitempty"`
	ContextID string `json:"context_id,omitempty"`
}

// DefaultConfig returns Claw's in-process defaults without reading
// workspace configuration files.
func DefaultConfig() Config {
	return defaultConfig()
}

func defaultConfig() Config {
	return Config{
		Workspace: WorkspaceConfig{
			MemoryRoot:  "memory",
			StateRoot:   "state",
			HistoryRoot: "history",
		},
		Conversation: ConversationConfig{
			Starts:    "peer",
			ContextID: defaultConversationContextID,
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
		History: HistoryConfig{
			Enabled:     false,
			Kind:        "buffer",
			MaxMessages: 50,
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
				Enabled:  boolPtr(true),
				TopK:     5,
				Inject:   "board",
				BoardVar: "memory_context",
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

func loadConfig(ctx context.Context, ws sdkworkspace.Workspace) (Config, error) {
	cfg := defaultConfig()
	if ws == nil {
		return Config{}, fmt.Errorf("claw: workspace is nil")
	}
	if err := readYAMLConfigFile(ctx, ws, "config.yaml", &cfg); err != nil {
		return Config{}, err
	}
	ExpandSettingsEnv(&cfg.Settings)
	ExpandModelsEnv(&cfg.Models)
	cfg.applyDefaults()
	return cfg, nil
}

func readYAMLConfigFile(ctx context.Context, ws sdkworkspace.Workspace, path string, out any) error {
	raw, err := ws.Read(ctx, path)
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return fmt.Errorf("claw: required config %s not found: %w", path, err)
		}
		return err
	}
	var generic any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return fmt.Errorf("claw: decode %s: %w", path, err)
	}
	jsonRaw, err := json.Marshal(normalizeYAMLValue(generic))
	if err != nil {
		return fmt.Errorf("claw: normalize %s: %w", path, err)
	}
	if err := json.Unmarshal(jsonRaw, out); err != nil {
		return fmt.Errorf("claw: decode %s: %w", path, err)
	}
	if cfg, ok := out.(*Config); ok {
		applyRawConfigExtensions(normalizeYAMLValue(generic), cfg)
	}
	return nil
}

func applyRawConfigExtensions(raw any, cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.Settings.empty() {
		cfg.Settings = legacyModelSettings(raw)
	}
	nodePolicies := graphNodePublishPolicies(raw)
	if len(nodePolicies) == 0 {
		return
	}
	if cfg.Agent.Publisher.Nodes == nil {
		cfg.Agent.Publisher.Nodes = map[string]NodePublishConfig{}
	}
	for nodeID, policy := range nodePolicies {
		cfg.Agent.Publisher.Nodes[nodeID] = policy
	}
}

func legacyModelSettings(raw any) ModelSettingsConfig {
	var settings ModelSettingsConfig
	root, ok := raw.(map[string]any)
	if !ok {
		return settings
	}
	modelsMap, ok := root["models"].(map[string]any)
	if !ok {
		return settings
	}
	settingsMap, ok := modelsMap["settings"].(map[string]any)
	if !ok {
		return settings
	}
	rawSettings, err := json.Marshal(settingsMap)
	if err != nil {
		return settings
	}
	_ = json.Unmarshal(rawSettings, &settings)
	return settings
}

func graphNodePublishPolicies(raw any) map[string]NodePublishConfig {
	root, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	agentMap, ok := root["agent"].(map[string]any)
	if !ok {
		return nil
	}
	graphMap, ok := agentMap["graph"].(map[string]any)
	if !ok {
		return nil
	}
	nodes, ok := graphMap["nodes"].([]any)
	if !ok {
		return nil
	}
	out := map[string]NodePublishConfig{}
	for _, rawNode := range nodes {
		nodeMap, ok := rawNode.(map[string]any)
		if !ok {
			continue
		}
		nodeID, _ := nodeMap["id"].(string)
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" {
			continue
		}
		publish, ok := boolValue(nodeMap["publish"])
		if !ok {
			continue
		}
		out[nodeID] = NodePublishConfig{Publish: &publish}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func boolValue(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
	}
	return false, false
}

func normalizeYAMLValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = normalizeYAMLValue(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[fmt.Sprint(k)] = normalizeYAMLValue(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalizeYAMLValue(item)
		}
		return out
	default:
		return v
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
	if strings.TrimSpace(c.Workspace.HistoryRoot) == "" {
		c.Workspace.HistoryRoot = def.Workspace.HistoryRoot
	}
	if strings.TrimSpace(c.Conversation.Starts) == "" {
		c.Conversation.Starts = def.Conversation.Starts
	}
	if strings.TrimSpace(c.Conversation.ContextID) == "" {
		c.Conversation.ContextID = def.Conversation.ContextID
	}
	if strings.TrimSpace(c.Models.Chat) == "" ||
		(strings.TrimSpace(c.Models.Chat) == def.Models.Chat && strings.TrimSpace(c.Settings.GenerateModel) != "") {
		c.Models.Chat = firstNonEmpty(modelRoleRef(c.Settings.GenerateModel, "generate_model"), def.Models.Chat)
	}
	if strings.TrimSpace(c.Models.Extractor) == "" && strings.TrimSpace(c.Settings.ExtractModel) != "" {
		c.Models.Extractor = "extract_model"
	}
	if strings.TrimSpace(c.Models.Embedder) == "" && strings.TrimSpace(c.Settings.EmbeddingModel) != "" {
		c.Models.Embedder = "embedding_model"
	}
	if c.Models.LLM == nil {
		c.Models.LLM = map[string]ModelConfig{}
	}
	if strings.TrimSpace(c.History.Kind) == "" {
		c.History.Kind = def.History.Kind
	}
	if c.History.MaxMessages <= 0 {
		c.History.MaxMessages = def.History.MaxMessages
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

func modelRoleRef(value, role string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return role
}

// ExpandModelsEnv expands environment variables only inside model
// settings and provider credentials. Unknown variables are preserved so
// unresolved graph variables or missing deployment secrets never collapse
// into empty strings silently.
func ExpandSettingsEnv(settings *ModelSettingsConfig) {
	if settings == nil {
		return
	}
	expandEnvValue(reflect.ValueOf(settings).Elem())
}

func ExpandModelsEnv(models *ModelsConfig) {
	if models == nil {
		return
	}
	expandEnvValue(reflect.ValueOf(models).Elem())
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
			v.SetString(expandEnvString(v.String()))
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
		return expandEnvString(x)
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

func expandEnvString(s string) string {
	return os.Expand(s, func(name string) string {
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		return "${" + name + "}"
	})
}
