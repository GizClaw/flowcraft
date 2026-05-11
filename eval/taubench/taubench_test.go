package taubench_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/eval/taubench"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// scriptedAgent is a deterministic stand-in for the agent LLM. Each
// call returns the next entry in `turns`. A turn is either a tool
// call (toolName + args) or a plain text reply (text).
type scriptedAgent struct {
	turns []scriptedTurn
	idx   int
}

type scriptedTurn struct {
	toolName string
	args     map[string]any
	text     string // when toolName is empty, agent ends with this text
}

func (s *scriptedAgent) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if s.idx >= len(s.turns) {
		// Defensive: return a plain "done" message if the script ran
		// out — better than nil so the agent loop terminates cleanly.
		return model.NewTextMessage(model.RoleAssistant, "done"), llm.TokenUsage{}, nil
	}
	t := s.turns[s.idx]
	s.idx++
	if t.toolName == "" {
		return model.NewTextMessage(model.RoleAssistant, t.text), llm.TokenUsage{}, nil
	}
	argsJSON, _ := json.Marshal(t.args)
	return llm.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{{
			Type: model.PartToolCall,
			ToolCall: &model.ToolCall{
				ID:        "call-" + t.toolName,
				Name:      t.toolName,
				Arguments: string(argsJSON),
			},
		}},
	}, llm.TokenUsage{}, nil
}

func (s *scriptedAgent) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

// TestRetailTools_Smoke exercises each handler against a fixed state.
// Sanity bar: returns expected shape, mutates the documented field,
// rejects status-protected operations.
func TestRetailTools_Smoke(t *testing.T) {
	tools := taubench.NewRetailTools()
	ds := taubench.NewRetailMiniDataset()
	state := ds.Tasks[0].InitialState

	// 1. get_order on a known id.
	out, err := tools["get_order"].Handler(state, map[string]any{"order_id": "ORD-1001"})
	if err != nil {
		t.Fatalf("get_order: %v", err)
	}
	if m, ok := out.(map[string]any); !ok || m["status"] != "pending" {
		t.Errorf("get_order: want status=pending, got %v", out)
	}

	// 2. cancel_order on a pending order should succeed.
	if _, err := tools["cancel_order"].Handler(state, map[string]any{"order_id": "ORD-1001", "reason": "test"}); err != nil {
		t.Fatalf("cancel_order: %v", err)
	}
	if m := state["orders"].(map[string]any)["ORD-1001"].(map[string]any); m["status"] != "cancelled" {
		t.Errorf("cancel did not flip status: %v", m["status"])
	}

	// 3. update_shipping on a delivered order should be refused.
	out, err = tools["update_shipping"].Handler(state, map[string]any{"order_id": "ORD-1003", "address": "x"})
	if err != nil {
		t.Fatalf("update_shipping (unexpected error path): %v", err)
	}
	if m, _ := out.(map[string]any); m["error"] == nil {
		t.Errorf("update_shipping should refuse delivered order, got %v", out)
	}

	// 4. search_products substring match.
	out, err = tools["search_products"].Handler(state, map[string]any{"query": "red"})
	if err != nil {
		t.Fatalf("search_products: %v", err)
	}
	if ids, _ := out.(map[string]any)["product_ids"].([]string); len(ids) != 1 || ids[0] != "P-1" {
		t.Errorf("search_products(\"red\"): want [P-1], got %v", ids)
	}
}

// TestRun_HappyPath drives the agent loop with a scripted agent that
// (a) calls cancel_order, (b) replies with a plain confirmation. The
// task's StateChecks should pass and the report should mark it as
// success.
func TestRun_HappyPath(t *testing.T) {
	agent := &scriptedAgent{turns: []scriptedTurn{
		{toolName: "cancel_order", args: map[string]any{"order_id": "ORD-1001", "reason": "test"}},
		{text: "Done — order ORD-1001 has been cancelled."},
	}}

	ds := &taubench.Dataset{
		Name:  "single",
		Tasks: []taubench.Task{taubench.NewRetailMiniDataset().Tasks[0]},
	}
	rep, err := taubench.Run(context.Background(), ds, taubench.Options{
		AgentLLM:    agent,
		Tools:       taubench.NewRetailTools(),
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.N != 1 || rep.Passed != 1 {
		t.Errorf("expected 1/1 pass, got %d/%d (reason: %s)", rep.Passed, rep.N, rep.Tasks[0].Reason)
	}
	if got := rep.Tasks[0].ToolCalls; len(got) != 1 || got[0] != "cancel_order" {
		t.Errorf("tool calls: want [cancel_order], got %v", got)
	}
}

// TestRun_StateMismatchFails: the agent calls the WRONG tool, so the
// state never reaches the expected end state. The report should
// reflect a failure with a state-mismatch reason.
func TestRun_StateMismatchFails(t *testing.T) {
	agent := &scriptedAgent{turns: []scriptedTurn{
		// Agent just reads the order, never cancels it.
		{toolName: "get_order", args: map[string]any{"order_id": "ORD-1001"}},
		{text: "I see your order. Is there anything else I can help with?"},
	}}
	ds := &taubench.Dataset{
		Name:  "single",
		Tasks: []taubench.Task{taubench.NewRetailMiniDataset().Tasks[0]},
	}
	rep, err := taubench.Run(context.Background(), ds, taubench.Options{
		AgentLLM:    agent,
		Tools:       taubench.NewRetailTools(),
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Passed != 0 {
		t.Errorf("expected 0/1 pass, got %d/%d", rep.Passed, rep.N)
	}
	if rep.Tasks[0].Success {
		t.Errorf("task should be marked unsuccessful")
	}
}

// TestRun_HookEvents pins the lifecycle event sequence so the Feishu
// adapter doesn't silently regress.
func TestRun_HookEvents(t *testing.T) {
	agent := &scriptedAgent{turns: []scriptedTurn{{text: "ok"}}}
	ds := &taubench.Dataset{
		Name:  "single",
		Tasks: []taubench.Task{taubench.NewRetailMiniDataset().Tasks[4]}, // product-search (no state mutation, no required tool)
	}
	var kinds []string
	_, err := taubench.Run(context.Background(), ds, taubench.Options{
		AgentLLM:    agent,
		Tools:       taubench.NewRetailTools(),
		Concurrency: 1,
		Hook: func(_ context.Context, e taubench.Event) {
			kinds = append(kinds, e.Kind)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"start", "done"}
	if len(kinds) != len(want) {
		t.Fatalf("events: want %v, got %v", want, kinds)
	}
	for i, w := range want {
		if kinds[i] != w {
			t.Errorf("event[%d]: want %q, got %q", i, w, kinds[i])
		}
	}
}
