package claw

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestLoadConfigUsesDomainFiles(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	ctx := context.Background()
	if err := ws.Write(ctx, "config/models.yaml", []byte(`
chat: fast
llm:
  fast:
    provider: mock
    model: mock-fast
`)); err != nil {
		t.Fatalf("Write models: %v", err)
	}
	if err := ws.Write(ctx, "config/agent.yaml", []byte(`
id: local-agent
name: Local Agent
system_prompt: stay concise
`)); err != nil {
		t.Fatalf("Write agent: %v", err)
	}

	cfg, err := loadConfig(ctx, ws)
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
	if cfg.Workspace.RecallRoot == "" || cfg.Workspace.RetrievalRoot == "" {
		t.Fatalf("workspace defaults were not applied: %+v", cfg.Workspace)
	}
}

func TestConfigExpandEnv(t *testing.T) {
	t.Setenv("CLAW_TEST_MODEL", "mock-fast")
	t.Setenv("CLAW_TEST_KEY", "secret")
	cfg := Config{
		Models: ModelsConfig{
			Chat: "chat",
			LLM: map[string]ModelConfig{
				"chat": {
					Provider: "mock",
					Model:    "${CLAW_TEST_MODEL}",
					APIKey:   "$CLAW_TEST_KEY",
					Config: map[string]any{
						"nested": []any{"${CLAW_TEST_MODEL}"},
					},
				},
			},
		},
	}
	cfg.ExpandEnv()
	if got := cfg.Models.LLM["chat"].Model; got != "mock-fast" {
		t.Fatalf("Model = %q, want mock-fast", got)
	}
	if got := cfg.Models.LLM["chat"].APIKey; got != "secret" {
		t.Fatalf("APIKey = %q, want secret", got)
	}
	nested := cfg.Models.LLM["chat"].Config["nested"].([]any)
	if got := nested[0]; got != "mock-fast" {
		t.Fatalf("nested env = %q, want mock-fast", got)
	}
}

func TestLocalSubWorkspaceRequiresLocalRoot(t *testing.T) {
	if _, err := localSubWorkspace(fakeWorkspace{}, "x"); err == nil {
		t.Fatal("localSubWorkspace succeeded for non-local workspace")
	}
}

type fakeWorkspace struct {
	workspace.Workspace
}
