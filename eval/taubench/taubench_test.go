package taubench_test

import (
	"context"
	"encoding/json"
	"strings"
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

// scriptedCustomer is the customer-side counterpart of scriptedAgent:
// it returns plain text replies in order, optionally terminating the
// dialog by including the stop token. The customer never produces
// tool calls.
type scriptedCustomer struct {
	replies []string
	idx     int
}

func (s *scriptedCustomer) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if s.idx >= len(s.replies) {
		// Exhausting the script silently emits an empty reply rather
		// than panicking; the harness's MaxConversationTurns will
		// catch a runaway dialog if the test forgot a stop entry.
		return model.NewTextMessage(model.RoleAssistant, ""), llm.TokenUsage{}, nil
	}
	r := s.replies[s.idx]
	s.idx++
	return model.NewTextMessage(model.RoleAssistant, r), llm.TokenUsage{}, nil
}

func (s *scriptedCustomer) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

// TestRun_DialogHappyPath drives one round-trip of the multi-turn
// harness:
//
//	customer opens  → agent calls cancel_order → agent says "done"
//	customer acks  → ###STOP### terminates the loop
//
// The task's StateCheck (status=cancelled) must pass, and the report
// must mark Mode="multi-turn" with both AgentTurns AND CustomerTurns
// > 0. Customer turns >= 1 (the "thanks ###STOP###" reply); opening
// utterance is hard-coded so it does NOT count as a customer-LLM
// call.
func TestRun_DialogHappyPath(t *testing.T) {
	task := taubench.NewRetailMiniDataset().Tasks[5] // retail-dialog-forgot-order-id
	// The agent must figure out the order id; we hardcode the
	// "right" sequence: list_orders_for_customer → cancel_order →
	// "I cancelled ORD-1001."
	agent := &scriptedAgent{turns: []scriptedTurn{
		{toolName: "list_orders_for_customer", args: map[string]any{"customer_id": "CUST-1"}},
		{toolName: "cancel_order", args: map[string]any{"order_id": "ORD-1001", "reason": "wrong size"}},
		{text: "Done — your pending order ORD-1001 has been cancelled."},
	}}
	// The scripted customer just acknowledges and terminates. The
	// opening utterance is deterministic (CustomerOpening on the
	// task), so it does NOT consume a customer LLM call — only the
	// follow-up reply does.
	customer := &scriptedCustomer{replies: []string{"Thanks! ###STOP###"}}

	ds := &taubench.Dataset{Name: "single", Tasks: []taubench.Task{task}}
	rep, err := taubench.Run(context.Background(), ds, taubench.Options{
		AgentLLM:    agent,
		CustomerLLM: customer,
		Tools:       taubench.NewRetailTools(),
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.N != 1 || rep.Passed != 1 {
		t.Fatalf("expected 1/1 pass, got %d/%d (reason: %s)", rep.Passed, rep.N, rep.Tasks[0].Reason)
	}
	got := rep.Tasks[0]
	if got.Mode != "multi-turn" {
		t.Errorf("Mode: want multi-turn, got %q", got.Mode)
	}
	if got.AgentTurns != 3 {
		t.Errorf("AgentTurns: want 3 (list, cancel, confirm), got %d", got.AgentTurns)
	}
	if got.CustomerTurns != 1 {
		t.Errorf("CustomerTurns: want 1 (the ###STOP### reply), got %d (opening doesn't count)", got.CustomerTurns)
	}
	if want, got := []string{"list_orders_for_customer", "cancel_order"}, got.ToolCalls; len(got) != len(want) {
		t.Errorf("ToolCalls: want %v, got %v", want, got)
	}
}

// TestRun_DialogMissingCustomerLLMFails covers the input-validation
// guard: a dataset with a multi-turn task BUT no CustomerLLM is a
// misconfiguration we want to fail loudly, not silently degrade.
func TestRun_DialogMissingCustomerLLMFails(t *testing.T) {
	task := taubench.NewRetailMiniDataset().Tasks[5]
	ds := &taubench.Dataset{Name: "single", Tasks: []taubench.Task{task}}
	_, err := taubench.Run(context.Background(), ds, taubench.Options{
		AgentLLM:    &scriptedAgent{},
		Tools:       taubench.NewRetailTools(),
		Concurrency: 1,
		// Deliberately no CustomerLLM.
	})
	if err == nil {
		t.Fatal("expected an error when multi-turn task is given without CustomerLLM, got nil")
	}
}

// TestRun_DialogConversationCap exercises the upper turn cap: a
// customer that NEVER emits the stop token must end up as a timeout
// failure, NOT a success, even if the underlying state-check would
// have passed.
func TestRun_DialogConversationCap(t *testing.T) {
	task := taubench.NewRetailMiniDataset().Tasks[5] // forgot-order-id
	agent := &scriptedAgent{turns: []scriptedTurn{
		{toolName: "cancel_order", args: map[string]any{"order_id": "ORD-1001", "reason": "test"}},
		{text: "Cancelled."},
		{text: "Anything else?"},
		{text: "Anything else?"},
		{text: "Anything else?"},
	}}
	customer := &scriptedCustomer{replies: []string{
		"Wait, can you double check?",
		"And the refund?",
		"Are you still there?",
		"Hello?",
	}}
	rep, err := taubench.Run(context.Background(), &taubench.Dataset{
		Name:  "single",
		Tasks: []taubench.Task{task},
	}, taubench.Options{
		AgentLLM:             agent,
		CustomerLLM:          customer,
		Tools:                taubench.NewRetailTools(),
		Concurrency:          1,
		MaxConversationTurns: 3, // tight cap to hit the limit fast
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := rep.Tasks[0]
	if got.Success {
		t.Errorf("task should NOT have succeeded; customer never said ###STOP###")
	}
	if !strings.Contains(got.Reason, "conversation did not terminate") {
		t.Errorf("Reason: want \"conversation did not terminate ...\", got %q", got.Reason)
	}
}

// TestAirlineTools_Smoke mirrors TestRetailTools_Smoke against the
// airline domain: each handler runs once, the status-protected paths
// refuse, and the integer-coercing helper accepts both float64 and
// stringified arguments (the most common provider quirks).
func TestAirlineTools_Smoke(t *testing.T) {
	tools := taubench.NewAirlineTools()
	state := taubench.NewAirlineMiniDataset().Tasks[0].InitialState

	// get_user → tier=gold (sanity).
	out, err := tools["get_user"].Handler(state, map[string]any{"user_id": "USER-1"})
	if err != nil {
		t.Fatalf("get_user: %v", err)
	}
	if m, _ := out.(map[string]any); m["tier"] != "gold" {
		t.Errorf("get_user: want tier=gold, got %v", out)
	}

	// cancel_reservation on a confirmed one succeeds.
	if _, err := tools["cancel_reservation"].Handler(state, map[string]any{"reservation_id": "RES-1", "reason": "test"}); err != nil {
		t.Fatalf("cancel_reservation: %v", err)
	}
	if got := state["reservations"].(map[string]any)["RES-1"].(map[string]any)["status"]; got != "cancelled" {
		t.Errorf("cancel did not flip status: %v", got)
	}

	// cancel_reservation on a departed flight is refused (returns
	// error payload, NOT a Go error).
	out, _ = tools["cancel_reservation"].Handler(state, map[string]any{"reservation_id": "RES-3", "reason": "test"})
	if m, _ := out.(map[string]any); m["error"] == nil {
		t.Errorf("cancel_reservation on departed: should return error payload, got %v", out)
	}

	// update_baggage accepts BOTH the canonical float64 (json
	// unmarshal default) and a stringified integer — this is the
	// most common provider quirk and the handler must tolerate it.
	for _, n := range []any{2.0, "2", 2} {
		state := taubench.NewAirlineMiniDataset().Tasks[0].InitialState // fresh state per case
		out, err := tools["update_baggage"].Handler(state, map[string]any{"reservation_id": "RES-1", "num_bags": n})
		if err != nil {
			t.Errorf("update_baggage(num_bags=%v): %v", n, err)
			continue
		}
		if m, _ := out.(map[string]any); m["error"] != nil {
			t.Errorf("update_baggage(num_bags=%v): want success, got error %v", n, m["error"])
			continue
		}
		got := state["reservations"].(map[string]any)["RES-1"].(map[string]any)["baggage"].(map[string]any)["checked"]
		if got != 2 {
			t.Errorf("update_baggage(num_bags=%v): want baggage.checked=2, got %v", n, got)
		}
	}

	// update_baggage out of range refused.
	out, _ = tools["update_baggage"].Handler(state, map[string]any{"reservation_id": "RES-2", "num_bags": 5.0})
	if m, _ := out.(map[string]any); m["error"] == nil {
		t.Errorf("update_baggage(num_bags=5): should return error payload, got %v", out)
	}

	// search_flight returns 2 candidates on JFK→LAX 2026-06-01.
	out, _ = tools["search_flight"].Handler(state, map[string]any{"origin": "JFK", "destination": "LAX", "date": "2026-06-01"})
	if ids, _ := out.(map[string]any)["flight_ids"].([]string); len(ids) != 2 {
		t.Errorf("search_flight: want 2 flight_ids, got %v", ids)
	}
}

// TestMergeDatasets sanity-checks the helper used by --domain all:
// names propagate, every task is preserved, duplicate IDs panic.
func TestMergeDatasets(t *testing.T) {
	retail := taubench.NewRetailMiniDataset()
	airline := taubench.NewAirlineMiniDataset()
	merged := taubench.MergeDatasets("retail+airline", retail, airline)
	if merged.Name != "retail+airline" {
		t.Errorf("Name: want retail+airline, got %q", merged.Name)
	}
	if got, want := len(merged.Tasks), len(retail.Tasks)+len(airline.Tasks); got != want {
		t.Errorf("task count: want %d, got %d", want, got)
	}

	// Duplicate ID -> panic.
	defer func() {
		if recover() == nil {
			t.Error("MergeDatasets with duplicate IDs did not panic")
		}
	}()
	dup := &taubench.Dataset{Tasks: []taubench.Task{retail.Tasks[0], retail.Tasks[0]}}
	_ = taubench.MergeDatasets("dup", dup)
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
