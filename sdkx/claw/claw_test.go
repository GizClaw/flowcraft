package claw

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

var testProviderSeq atomic.Uint64

func newTestClaw(t *testing.T, ws sdkworkspace.Workspace, client llm.LLM, mutate func(*Config)) *Claw {
	t.Helper()
	cfg := testConfigForLLM(t, client)
	if mutate != nil {
		mutate(&cfg)
	}
	writeTestConfig(t, ws, cfg)
	app, err := New(ws)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return app
}

func testConfigForLLM(t *testing.T, client llm.LLM) Config {
	t.Helper()
	provider := fmt.Sprintf("clawtest%d", testProviderSeq.Add(1))
	llm.RegisterProvider(provider, func(string, map[string]any) (llm.LLM, error) {
		return client, nil
	})
	cfg := defaultConfig()
	cfg.Models.Chat = "chat"
	cfg.Models.LLM = map[string]ModelConfig{
		"chat": {Provider: provider, Model: "chat"},
	}
	cfg.Memory.Enabled = false
	cfg.Agent.Model = "chat"
	return cfg
}

func writeTestConfig(t *testing.T, ws sdkworkspace.Workspace, cfg Config) {
	t.Helper()
	ctx := context.Background()
	files := map[string]any{
		"config.yaml": testConfigFile(cfg),
	}
	for path, value := range files {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", path, err)
		}
		if err := ws.Write(ctx, path, raw); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func testConfigFile(cfg Config) any {
	return struct {
		Workspace    WorkspaceConfig     `json:"workspace,omitempty"`
		Conversation ConversationConfig  `json:"conversation,omitempty"`
		Settings     ModelSettingsConfig `json:"settings,omitempty"`
		Models       ModelsConfig        `json:"models,omitempty"`
		History      HistoryConfig       `json:"history,omitempty"`
		Memory       any                 `json:"memory,omitempty"`
		Agent        AgentConfig         `json:"agent,omitempty"`
	}{
		Workspace:    cfg.Workspace,
		Conversation: cfg.Conversation,
		Settings:     cfg.Settings,
		Models:       cfg.Models,
		History:      cfg.History,
		Memory:       testMemoryConfig(cfg.Memory),
		Agent:        cfg.Agent,
	}
}

func testMemoryConfig(cfg MemoryConfig) any {
	return struct {
		Enabled   bool                  `json:"enabled,omitempty"`
		Scope     MemoryScopeConfig     `json:"scope,omitempty"`
		Write     MemoryWriteConfig     `json:"write,omitempty"`
		Extract   MemoryExtractConfig   `json:"extract,omitempty"`
		Layout    MemoryLayoutConfig    `json:"layout,omitempty"`
		Recall    MemoryRecallConfig    `json:"recall,omitempty"`
		Retrieval testRetrievalConfig   `json:"retrieval,omitempty"`
		Embedding MemoryEmbeddingConfig `json:"embedding,omitempty"`
	}{
		Enabled: cfg.Enabled,
		Scope:   cfg.Scope,
		Write:   cfg.Write,
		Extract: cfg.Extract,
		Layout:  cfg.Layout,
		Recall:  cfg.Recall,
		Retrieval: testRetrievalConfig{
			Backend: cfg.Retrieval.Backend,
		},
		Embedding: cfg.Embedding,
	}
}

type testRetrievalConfig struct {
	Backend string `json:"backend,omitempty"`
}
