package claw

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestDefaultConfigReturnsUsableDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Workspace.MemoryRoot == "" || cfg.Workspace.StateRoot == "" {
		t.Fatalf("workspace defaults were not applied: %+v", cfg.Workspace)
	}
	if cfg.Models.Chat == "" {
		t.Fatal("Models.Chat is empty")
	}
	if cfg.Agent.ID == "" {
		t.Fatal("Agent.ID is empty")
	}
	if cfg.Conversation.Starts != "peer" {
		t.Fatalf("Conversation.Starts = %q, want peer", cfg.Conversation.Starts)
	}
}

func TestLoadConfigUsesFixedWorkspaceJSONFiles(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Setenv("CLAW_TEST_MODEL", "mock-fast")
	cfg := defaultConfig()
	cfg.Models.Chat = "fast"
	cfg.Models.LLM = map[string]ModelConfig{
		"fast": {
			Provider: "mock",
			Model:    "${CLAW_TEST_MODEL}",
		},
	}
	cfg.Agent.ID = "local-agent"
	cfg.Agent.Name = "Local Agent"
	cfg.Agent.SystemPrompt = "stay concise"
	cfg.Conversation.Starts = "self"
	writeTestConfig(t, ws, cfg)

	got, err := loadConfig(context.Background(), ws)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.Models.Chat != "fast" {
		t.Fatalf("Models.Chat = %q, want fast", got.Models.Chat)
	}
	if got.Models.LLM["fast"].Model != "mock-fast" {
		t.Fatalf("fast model = %q, want mock-fast", got.Models.LLM["fast"].Model)
	}
	if got.Agent.ID != "local-agent" {
		t.Fatalf("Agent.ID = %q, want local-agent", got.Agent.ID)
	}
	if got.Workspace.MemoryRoot == "" || got.Workspace.StateRoot == "" {
		t.Fatalf("workspace defaults were not applied: %+v", got.Workspace)
	}
	if got.Conversation.Starts != "self" {
		t.Fatalf("Conversation.Starts = %q, want self", got.Conversation.Starts)
	}
}

func TestLoadConfigPreservesGraphVariableRefsDuringEnvExpansion(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Setenv("CLAW_TEST_MODEL", "mock-fast")
	cfg := defaultConfig()
	cfg.Models.Chat = "fast"
	cfg.Models.LLM = map[string]ModelConfig{
		"fast": {
			Provider: "mock",
			Model:    "${CLAW_TEST_MODEL}",
		},
	}
	cfg.Agent.Graph = graph.GraphDefinition{
		Name:  "test",
		Entry: "answer",
		Nodes: []graph.NodeDefinition{{
			ID:   "answer",
			Type: "llm",
			Config: map[string]any{
				"model":         "fast",
				"system_prompt": "${board.system_prompt}",
			},
		}},
		Edges: []graph.EdgeDefinition{{From: "answer", To: graph.END}},
	}
	writeTestConfig(t, ws, cfg)

	got, err := loadConfig(context.Background(), ws)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.Models.LLM["fast"].Model != "mock-fast" {
		t.Fatalf("fast model = %q, want mock-fast", got.Models.LLM["fast"].Model)
	}
	systemPrompt, _ := got.Agent.Graph.Nodes[0].Config["system_prompt"].(string)
	if systemPrompt != "${board.system_prompt}" {
		t.Fatalf("system_prompt = %q, want board ref preserved", systemPrompt)
	}
}

func TestLoadConfigExtractsGraphNodePublishPolicy(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	raw := []byte(`
agent:
  id: local-agent
  graph:
    name: match
    entry: format_intent
    nodes:
      - id: format_intent
        type: llm
        publish: false
        config:
          model: default
      - id: answer
        type: llm
        publish: true
        config:
          model: default
    edges:
      - from: format_intent
        to: answer
      - from: answer
        to: __end__
`)
	if err := ws.Write(context.Background(), "config.yaml", raw); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	got, err := loadConfig(context.Background(), ws)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	formatPolicy := got.Agent.Publisher.Nodes["format_intent"]
	if formatPolicy.Publish == nil || *formatPolicy.Publish {
		t.Fatalf("format_intent publish policy = %+v, want false", formatPolicy.Publish)
	}
	answerPolicy := got.Agent.Publisher.Nodes["answer"]
	if answerPolicy.Publish == nil || !*answerPolicy.Publish {
		t.Fatalf("answer publish policy = %+v, want true", answerPolicy.Publish)
	}
}

func TestLoadConfigRequiresFixedFiles(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	_, err = New(ws)
	if err == nil {
		t.Fatal("New succeeded without config files")
	}
	if !strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("New error = %v, want missing workspace config", err)
	}
}
