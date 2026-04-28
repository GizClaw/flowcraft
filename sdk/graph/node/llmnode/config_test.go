package llmnode

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func newTestBoard() *graph.Board { return graph.NewBoard() }

func mustConfigFromMap(t *testing.T, m map[string]any) Config {
	t.Helper()
	cfg, err := ConfigFromMap(m, nil)
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
	if !cfg.TrackSteps {
		t.Fatal("TrackSteps should be true")
	}
}

func TestConfigFromMap_Defaults(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{})
	if cfg.SystemPrompt != "" || cfg.Temperature != nil || cfg.MaxTokens != 0 {
		t.Fatalf("expected zero-value config, got %+v", cfg)
	}
}

func TestConfigFromMap_NilMap(t *testing.T) {
	cfg := mustConfigFromMap(t, nil)
	if cfg.SystemPrompt != "" || cfg.Model != "" {
		t.Fatal("nil map should produce zero-value config")
	}
}

func TestConfigFromMap_NumericStrings(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{
		"temperature": "0.8",
		"max_tokens":  "2048",
		"json_mode":   "true",
	})
	if cfg.Temperature == nil || *cfg.Temperature != 0.8 {
		t.Fatalf("Temperature = %v", cfg.Temperature)
	}
	if cfg.MaxTokens != 2048 {
		t.Fatalf("MaxTokens = %d", cfg.MaxTokens)
	}
	if !cfg.JSONMode {
		t.Fatal("JSONMode should be true")
	}
}

func TestConfigFromMap_TemplateRefDeferred(t *testing.T) {
	cfg, err := ConfigFromMap(map[string]any{
		"temperature":   "${board.temperature}",
		"max_tokens":    "${board.max_tokens}",
		"json_mode":     "${board.json_mode}",
		"system_prompt": "${board.system_prompt}",
	}, variable.ContainsRef)
	if err != nil {
		t.Fatalf("template ref should not error at build time: %v", err)
	}
	if cfg.Temperature != nil {
		t.Fatalf("template ref should leave Temperature nil, got %v", *cfg.Temperature)
	}
	if cfg.MaxTokens != 0 {
		t.Fatalf("template ref should leave MaxTokens 0, got %d", cfg.MaxTokens)
	}
	if cfg.JSONMode {
		t.Fatal("template ref should leave JSONMode false")
	}
	if cfg.SystemPrompt != "${board.system_prompt}" {
		t.Fatalf("template ref in string field should be kept, got %q", cfg.SystemPrompt)
	}
}

func TestConfigFromMap_InvalidString(t *testing.T) {
	_, err := ConfigFromMap(map[string]any{"temperature": "not-a-number"}, nil)
	if err == nil {
		t.Fatal("invalid string should produce an error")
	}
}

func TestConfigFromMap_ToolNames(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{
		"tool_names": []any{"search", "calculator"},
	})
	if len(cfg.ToolNames) != 2 || cfg.ToolNames[0] != "search" {
		t.Fatalf("ToolNames = %v", cfg.ToolNames)
	}
}

func TestConfigFromMap_Thinking(t *testing.T) {
	cfg := mustConfigFromMap(t, map[string]any{"thinking": true})
	if !cfg.Thinking {
		t.Fatal("Thinking should be true")
	}
}

func TestNode_Identity(t *testing.T) {
	n := New("llm1", nil, nil, Config{})
	if n.ID() != "llm1" {
		t.Fatalf("ID = %q", n.ID())
	}
	if n.Type() != "llm" {
		t.Fatalf("Type = %q", n.Type())
	}
	if len(n.InputPorts()) != 1 {
		t.Fatalf("InputPorts len = %d", len(n.InputPorts()))
	}
	if len(n.OutputPorts()) != 4 {
		t.Fatalf("OutputPorts len = %d", len(n.OutputPorts()))
	}
}

func TestNode_OutputPortCustomKey(t *testing.T) {
	n := New("llm1", nil, nil, Config{OutputKey: "custom_out"})
	if n.OutputPorts()[0].Name != "custom_out" {
		t.Fatalf("first output port = %q", n.OutputPorts()[0].Name)
	}
}

func TestNode_SetConfig(t *testing.T) {
	n := New("llm1", nil, nil, Config{SystemPrompt: "old"})
	n.SetConfig(map[string]any{"system_prompt": "new", "temperature": 0.8})
	if n.config.SystemPrompt != "new" {
		t.Fatalf("SystemPrompt after SetConfig = %q", n.config.SystemPrompt)
	}
	if n.config.Temperature == nil || *n.config.Temperature != 0.8 {
		t.Fatalf("Temperature after SetConfig = %v", n.config.Temperature)
	}
}

func TestBuildMessages_NoSystemDuplicate(t *testing.T) {
	n := New("n", nil, nil, Config{SystemPrompt: "be helpful"})
	board := newTestBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleSystem, "existing system"),
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	msgs := n.buildMessages(n.config, board, graph.MainChannel, graph.VarMessages)
	systemCount := 0
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			systemCount++
		}
	}
	if systemCount != 1 {
		t.Fatalf("expected 1 system message, got %d", systemCount)
	}
}

func TestBuildMessages_FallbackToVar(t *testing.T) {
	n := New("n", nil, nil, Config{})
	board := newTestBoard()
	board.SetVar(graph.VarMessages, []model.Message{
		model.NewTextMessage(model.RoleUser, "from var"),
	})

	msgs := n.buildMessages(n.config, board, "empty_channel", graph.VarMessages)
	if len(msgs) != 1 || msgs[0].Content() != "from var" {
		t.Fatalf("expected message from var, got %v", msgs)
	}
}

func TestBuildMessages_SummaryIndexInjection(t *testing.T) {
	board := newTestBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
	})
	board.SetVar(VarSummaryIndex, "## 摘要\n[s1] seq 0-10")

	n := New("n", nil, nil, Config{SystemPrompt: "You are helpful."})
	msgs := n.buildMessages(n.config, board, graph.MainChannel, graph.VarMessages)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	sysContent := msgs[0].Content()
	if !strings.Contains(sysContent, "You are helpful.") || !strings.Contains(sysContent, "[s1]") {
		t.Fatalf("system prompt missing summary injection: %q", sysContent)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Fatalf("short = %q", got)
	}
	if got := truncate("hello world", 5); got != "hello..." {
		t.Fatalf("long = %q", got)
	}
	if got := truncate("hello", 5); got != "hello" {
		t.Fatalf("exact = %q", got)
	}
}
