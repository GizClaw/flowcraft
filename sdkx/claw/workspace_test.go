package claw

import (
	"context"
	"testing"

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
}

func TestLoadConfigUsesWorkspaceJSONFiles(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	ctx := context.Background()
	if err := ws.Write(ctx, "config/models.json", []byte(`{
  "chat": "fast",
  "llm": {
    "fast": {
      "provider": "mock",
      "model": "mock-fast"
    }
  }
}`)); err != nil {
		t.Fatalf("Write models: %v", err)
	}
	if err := ws.Write(ctx, "config/agent.json", []byte(`{
  "id": "local-agent",
  "name": "Local Agent",
  "system_prompt": "stay concise"
}`)); err != nil {
		t.Fatalf("Write agent: %v", err)
	}

	cfg, err := loadConfig(ctx, ws, "config")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Models.Chat != "fast" {
		t.Fatalf("Models.Chat = %q, want fast", cfg.Models.Chat)
	}
	if cfg.Models.LLM["fast"].Model != "mock-fast" {
		t.Fatalf("fast model = %q, want mock-fast", cfg.Models.LLM["fast"].Model)
	}
	if cfg.Agent.ID != "local-agent" {
		t.Fatalf("Agent.ID = %q, want local-agent", cfg.Agent.ID)
	}
	if cfg.Workspace.MemoryRoot == "" || cfg.Workspace.StateRoot == "" {
		t.Fatalf("workspace defaults were not applied: %+v", cfg.Workspace)
	}
}

func TestConfigOptionsOverrideWorkspaceJSON(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	if err := ws.Write(context.Background(), "config/models.json", []byte(`{
  "chat": "from-file",
  "llm": {
    "from-file": {
      "provider": "mock",
      "model": "mock-file"
    }
  }
}`)); err != nil {
		t.Fatalf("Write models: %v", err)
	}

	app, err := New(ws, WithModels(ModelsConfig{
		Chat: "override",
		LLM: map[string]ModelConfig{
			"override": {Provider: "mock", Model: "mock-override"},
		},
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()
	cfg := app.Config()
	if cfg.Models.Chat != "override" {
		t.Fatalf("Models.Chat = %q, want override", cfg.Models.Chat)
	}
	if cfg.Models.LLM["override"].Model != "mock-override" {
		t.Fatalf("override model = %q, want mock-override", cfg.Models.LLM["override"].Model)
	}
}
