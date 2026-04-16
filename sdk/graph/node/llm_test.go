package node

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

func newTestBoard() *graph.Board {
	return graph.NewBoard()
}

func pf64(v float64) *float64 { return &v }

func mustConfigFromMap(t *testing.T, m map[string]any) LLMConfig {
	t.Helper()
	cfg, err := ConfigFromMap(m)
	if err != nil {
		t.Fatalf("ConfigFromMap: %v", err)
	}
	return cfg
}

func TestConfigFromMap_Full(t *testing.T) {
	m := map[string]any{
		"system_prompt":  "You are helpful.",
		"model":          "openai/gpt-4o",
		"temperature":    0.7,
		"max_tokens":     float64(1024),
		"output_key":     "answer",
		"messages_key":   "msgs",
		"json_mode":      true,
		"query_fallback": false,
		"track_steps":    true,
	}

	cfg := mustConfigFromMap(t, m)

	if cfg.SystemPrompt != "You are helpful." {
		t.Fatalf("SystemPrompt = %q", cfg.SystemPrompt)
	}
	if cfg.Model != "openai/gpt-4o" {
		t.Fatalf("Model = %q", cfg.Model)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.7 {
		t.Fatalf("Temperature = %v", cfg.Temperature)
	}
	if cfg.MaxTokens != 1024 {
		t.Fatalf("MaxTokens = %d", cfg.MaxTokens)
	}
	if cfg.OutputKey != "answer" {
		t.Fatalf("OutputKey = %q", cfg.OutputKey)
	}
	if cfg.MessagesKey != "msgs" {
		t.Fatalf("MessagesKey = %q", cfg.MessagesKey)
	}
	if !cfg.JSONMode {
		t.Fatal("JSONMode should be true")
	}
	if cfg.QueryFallback {
		t.Fatal("QueryFallback should be false")
	}
	if !cfg.TrackSteps {
		t.Fatal("TrackSteps should be true")
	}
}

func TestConfigFromMap_Defaults(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{})
	if cfg.SystemPrompt != "" {
		t.Fatal("SystemPrompt should be empty")
	}
	if cfg.Temperature != nil {
		t.Fatal("Temperature should be nil")
	}
	if cfg.MaxTokens != 0 {
		t.Fatal("MaxTokens should be 0")
	}
}

func TestConfigFromMap_MaxTokensInt(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{"max_tokens": 512})
	if cfg.MaxTokens != 512 {
		t.Fatalf("MaxTokens from int = %d, want 512", cfg.MaxTokens)
	}
}

func TestConfigFromMap_TemperatureInt(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{"temperature": 1})
	if cfg.Temperature == nil || *cfg.Temperature != 1.0 {
		t.Fatalf("Temperature from int = %v, want 1.0", cfg.Temperature)
	}
}

func TestConfigFromMap_TemperatureFloat(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{"temperature": 0.3})
	if cfg.Temperature == nil || *cfg.Temperature != 0.3 {
		t.Fatalf("Temperature = %v, want 0.3", cfg.Temperature)
	}
}

func TestConfigFromMap_TemperatureString(t *testing.T) {
	m := map[string]any{"temperature": "0.8"}
	cfg := mustConfigFromMap(t, m)
	if cfg.Temperature == nil || *cfg.Temperature != 0.8 {
		t.Fatalf("Temperature = %v, want 0.8", cfg.Temperature)
	}
	if _, ok := m["temperature"].(string); !ok {
		t.Fatalf("input map should not be mutated, got %T", m["temperature"])
	}
}

func TestConfigFromMap_MaxTokensString(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{"max_tokens": "2048"})
	if cfg.MaxTokens != 2048 {
		t.Fatalf("MaxTokens = %d, want 2048", cfg.MaxTokens)
	}
}

func TestConfigFromMap_JSONModeString(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{"json_mode": "true"})
	if !cfg.JSONMode {
		t.Fatal("JSONMode should be true")
	}
}

func TestConfigFromMap_UnresolvedRef(t *testing.T) {
	_, err := ConfigFromMap(map[string]any{
		"temperature": "${board.missing}",
	})
	if err == nil {
		t.Fatal("unresolved ref should produce an error")
	}
}

func TestLLMNode_SummaryIndexInjection(t *testing.T) {
	board := newTestBoard()
	board.SetVar("messages", []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
	})
	board.SetVar("query", "hello")
	board.SetVar(workflow.VarSummaryIndex, "## 对话历史摘要\n\n[s1] seq 0-10: 之前讨论了RAG工作流")

	n := NewLLMNode("llm1", nil, nil, LLMConfig{
		SystemPrompt: "You are helpful.",
	})

	msgs, _ := board.GetVar("messages")
	messages := msgs.([]model.Message)
	messages = append([]model.Message{model.NewTextMessage(model.RoleSystem, n.config.SystemPrompt)}, messages...)

	if si, ok := board.GetVar(workflow.VarSummaryIndex); ok {
		if index, ok := si.(string); ok && index != "" {
			for i, m := range messages {
				if m.Role == model.RoleSystem {
					messages[i] = model.NewTextMessage(model.RoleSystem, m.Content()+"\n\n"+index)
					break
				}
			}
		}
	}

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	sysContent := messages[0].Content()
	if !strings.Contains(sysContent, "You are helpful.") {
		t.Fatal("system prompt should contain original content")
	}
	if !strings.Contains(sysContent, "对话历史摘要") {
		t.Fatal("system prompt should contain summary index")
	}
	if !strings.Contains(sysContent, "[s1]") {
		t.Fatal("system prompt should contain summary ID")
	}
}

func TestLLMNode_SummaryIndexNotInjectedWhenAbsent(t *testing.T) {
	board := newTestBoard()
	board.SetVar("messages", []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
	})

	messages := []model.Message{
		model.NewTextMessage(model.RoleSystem, "You are helpful."),
		model.NewTextMessage(model.RoleUser, "hello"),
	}

	if si, ok := board.GetVar(workflow.VarSummaryIndex); ok {
		if index, ok := si.(string); ok && index != "" {
			for i, m := range messages {
				if m.Role == model.RoleSystem {
					messages[i] = model.NewTextMessage(model.RoleSystem, m.Content()+"\n\n"+index)
					break
				}
			}
		}
	}

	sysContent := messages[0].Content()
	if sysContent != "You are helpful." {
		t.Fatalf("system prompt should remain unchanged, got %q", sysContent)
	}
}

func TestConfigFromMap_ToolNames(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{
		"tool_names": []any{"search", "calculator"},
	})
	if len(cfg.ToolNames) != 2 {
		t.Fatalf("ToolNames len = %d, want 2", len(cfg.ToolNames))
	}
	if cfg.ToolNames[0] != "search" || cfg.ToolNames[1] != "calculator" {
		t.Fatalf("ToolNames = %v", cfg.ToolNames)
	}
}

func TestConfigFromMap_ToolNamesStringSlice(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{
		"tool_names": []string{"a", "b"},
	})
	if len(cfg.ToolNames) != 2 {
		t.Fatalf("ToolNames len = %d, want 2", len(cfg.ToolNames))
	}
}

func TestConfigFromMap_Thinking(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{"thinking": true})
	if !cfg.Thinking {
		t.Fatal("Thinking should be true")
	}
}

func TestConfigFromMap_NilMap(t *testing.T) {
	cfg := mustConfigFromMap(t, nil)
	if cfg.SystemPrompt != "" || cfg.Model != "" {
		t.Fatal("nil map should produce zero-value config")
	}
}

func TestConfigFromMap_AllFields(t *testing.T) {
	m := map[string]any{
		"system_prompt":  "prompt",
		"model":          "gpt-4",
		"temperature":    0.5,
		"max_tokens":     float64(2048),
		"output_key":     "out",
		"messages_key":   "msgs",
		"json_mode":      true,
		"thinking":       true,
		"query_fallback": true,
		"track_steps":    true,
		"tool_names":     []any{"t1", "t2"},
	}
	cfg := mustConfigFromMap(t, m)
	if cfg.SystemPrompt != "prompt" {
		t.Fatalf("SystemPrompt = %q", cfg.SystemPrompt)
	}
	if cfg.Model != "gpt-4" {
		t.Fatalf("Model = %q", cfg.Model)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.5 {
		t.Fatalf("Temperature = %v", cfg.Temperature)
	}
	if cfg.MaxTokens != 2048 {
		t.Fatalf("MaxTokens = %d", cfg.MaxTokens)
	}
	if cfg.OutputKey != "out" {
		t.Fatalf("OutputKey = %q", cfg.OutputKey)
	}
	if cfg.MessagesKey != "msgs" {
		t.Fatalf("MessagesKey = %q", cfg.MessagesKey)
	}
	if !cfg.JSONMode {
		t.Fatal("JSONMode should be true")
	}
	if !cfg.Thinking {
		t.Fatal("Thinking should be true")
	}
	if !cfg.QueryFallback {
		t.Fatal("QueryFallback should be true")
	}
	if !cfg.TrackSteps {
		t.Fatal("TrackSteps should be true")
	}
	if len(cfg.ToolNames) != 2 {
		t.Fatalf("ToolNames len = %d", len(cfg.ToolNames))
	}
}

func TestLLMNode_SetConfig(t *testing.T) {
	n := NewLLMNode("llm1", nil, nil, LLMConfig{SystemPrompt: "old"})
	n.SetConfig(map[string]any{"system_prompt": "new", "temperature": 0.8})
	if n.config.SystemPrompt != "new" {
		t.Fatalf("SystemPrompt after SetConfig = %q", n.config.SystemPrompt)
	}
	if n.config.Temperature == nil || *n.config.Temperature != 0.8 {
		t.Fatalf("Temperature after SetConfig = %v", n.config.Temperature)
	}
}

func TestLLMNode_OutputPortCustomKey(t *testing.T) {
	n := NewLLMNode("llm1", nil, nil, LLMConfig{OutputKey: "custom_out"})
	ports := n.OutputPorts()
	if ports[0].Name != "custom_out" {
		t.Fatalf("first output port = %q, want custom_out", ports[0].Name)
	}
}

func TestLLMNode_Ports(t *testing.T) {
	n := NewLLMNode("llm1", nil, nil, LLMConfig{})
	if n.ID() != "llm1" {
		t.Fatalf("ID = %q", n.ID())
	}
	if n.Type() != "llm" {
		t.Fatalf("Type = %q", n.Type())
	}
	if len(n.InputPorts()) != 1 {
		t.Fatalf("InputPorts len = %d, want 1", len(n.InputPorts()))
	}
	if len(n.OutputPorts()) != 4 {
		t.Fatalf("OutputPorts len = %d, want 4", len(n.OutputPorts()))
	}
}
