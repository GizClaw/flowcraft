package claw

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestDebugHTTPWorkspaceAndHistory(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, func(cfg *Config) {
		cfg.Agent.ID = "debug-agent"
		cfg.Agent.Name = "Debug Agent"
		cfg.History.Enabled = true
		cfg.History.Kind = "buffer"
		cfg.Conversation.ContextID = "debug-context"
	})
	defer app.Close()

	if err := app.history.appendTurn(
		context.Background(),
		"debug-context",
		model.NewTextMessage(model.RoleUser, "hello"),
		[]model.Message{model.NewTextMessage(model.RoleAssistant, "hi")},
	); err != nil {
		t.Fatalf("append history: %v", err)
	}

	server := httptest.NewServer(NewDebugHTTPHandler(app))
	defer server.Close()

	resp, err := http.Get(server.URL + "/debug/workspace")
	if err != nil {
		t.Fatalf("GET workspace debug: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workspace status = %d, want 200", resp.StatusCode)
	}
	var workspaceResp debugWorkspaceResponse
	if err := json.NewDecoder(resp.Body).Decode(&workspaceResp); err != nil {
		t.Fatalf("decode workspace debug: %v", err)
	}
	if workspaceResp.Agent.ID != "debug-agent" || workspaceResp.Workspace.Root == "" {
		t.Fatalf("workspace debug = %+v, want agent and local root", workspaceResp)
	}
	if !workspaceResp.History.Enabled {
		t.Fatalf("history debug enabled = false, want true")
	}

	resp, err = http.Get(server.URL + "/debug/history?context_id=debug-context")
	if err != nil {
		t.Fatalf("GET history debug: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d, want 200", resp.StatusCode)
	}
	var historyResp debugHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&historyResp); err != nil {
		t.Fatalf("decode history debug: %v", err)
	}
	if !historyResp.Enabled || historyResp.ContextID != "debug-context" || historyResp.Count != 2 {
		t.Fatalf("history debug = %+v, want two messages in debug-context", historyResp)
	}
	if historyResp.Messages[0].Content() != "hello" || historyResp.Messages[1].Content() != "hi" {
		t.Fatalf("history messages = %+v, want hello/hi", historyResp.Messages)
	}
}

func TestDebugHTTPRecallUsesMemoryRuntime(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, func(cfg *Config) {
		cfg.Memory.Enabled = true
		cfg.Memory.Scope.RuntimeID = "rt"
		cfg.Memory.Scope.UserID = "user"
		cfg.Memory.Scope.AgentID = "agent"
		cfg.Memory.Write.Mode = "sync"
		cfg.Memory.Retrieval.Backend = "bbh"
		cfg.Memory.Recall.TopK = 3
	})
	defer app.Close()
	if app.memory == nil {
		t.Fatal("memory runtime is nil")
	}
	now := time.Now()
	if _, err := app.memory.mem.Save(context.Background(), app.memory.scope, recall.SaveRequest{
		Facts: []recall.TemporalFact{{
			Kind:       recall.FactPreference,
			Content:    "user_preferences: Tom likes fast scenes.",
			Subject:    "tom",
			Predicate:  "likes",
			Object:     "fast scenes",
			ObservedAt: now,
			ValidFrom:  &now,
			Confidence: 0.9,
		}},
		Mode: recall.WriteModeSync,
	}); err != nil {
		t.Fatalf("save memory fact: %v", err)
	}
	if err := app.memory.drainSideEffects(context.Background()); err != nil {
		t.Fatalf("drain memory side effects: %v", err)
	}

	body := bytes.NewBufferString(`{"text":"Tom likes fast scenes","top_k":1,"lanes":["user_preferences"]}`)
	req := httptest.NewRequest(http.MethodPost, "/debug/recall", body)
	rec := httptest.NewRecorder()
	app.ServeDebugHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("recall status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var recallResp debugRecallResponse
	if err := json.NewDecoder(rec.Body).Decode(&recallResp); err != nil {
		t.Fatalf("decode recall debug: %v", err)
	}
	if !recallResp.Enabled || recallResp.Count != 1 {
		t.Fatalf("recall debug = %+v, want one enabled hit", recallResp)
	}
	if got := recallResp.Hits[0].Content; got != "user_preferences: Tom likes fast scenes." {
		t.Fatalf("recall hit = %q, want saved fact", got)
	}
}
