package taubench_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/eval/taubench"
)

// retailInitialStateForUpstream mirrors the retail-mini base state
// but exposed as a State value (not a closure). LoadUpstreamTasks
// expects an explicit State so the fixture and the loader's initial
// state can drift independently.
func retailInitialStateForUpstream() taubench.State {
	return taubench.State{
		"customers": map[string]any{
			"CUST-1": map[string]any{"name": "Ada Lovelace", "email": "ada@example.com"},
		},
		"orders": map[string]any{
			"ORD-1001": map[string]any{
				"customer_id":      "CUST-1",
				"status":           "pending",
				"items":            []any{"P-1"},
				"shipping_address": "10 Computing Lane, London",
			},
		},
		"products": map[string]any{
			"P-1": map[string]any{"name": "Red Sneakers", "description": "Bright red running shoes", "price": 79.99},
		},
	}
}

// writeJSON marshals payload + writes to a temp file and returns the
// path. Tiny helper to keep the table-driven tests readable.
func writeJSON(t *testing.T, payload any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestLoadUpstreamTasks_ShadowRun verifies the end-to-end loader
// pipeline:
//
//  1. Read an upstream-shaped JSON file.
//  2. Shadow-execute each task's gold actions against initialState.
//  3. Snap the result as ExpectedFinalState on the produced Task.
//
// Then we drive Run with an agent that follows the gold trace EXACTLY
// — pass; and with an agent that calls the wrong tool — fail. This
// exercises both checkExpectedFinalState happy + sad paths.
func TestLoadUpstreamTasks_ShadowRun(t *testing.T) {
	tasksPayload := []map[string]any{
		{
			"user_id":     "CUST-1",
			"instruction": "Hi, I'm CUST-1. Please cancel order ORD-1001, reason: changed mind.",
			"actions": []map[string]any{
				{"name": "cancel_order", "kwargs": map[string]any{"order_id": "ORD-1001", "reason": "changed mind"}},
			},
			"outputs": []string{"cancelled"},
		},
	}
	tasksPath := writeJSON(t, tasksPayload)
	tools := taubench.NewRetailTools()
	initial := retailInitialStateForUpstream()

	tasks, err := taubench.LoadUpstreamTasks(tasksPath, initial, tools, "retail")
	if err != nil {
		t.Fatalf("LoadUpstreamTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	got := tasks[0]
	if got.ID != "retail-000" {
		t.Errorf("ID: want retail-000, got %q", got.ID)
	}
	if got.Expected.ExpectedFinalState == nil {
		t.Fatal("ExpectedFinalState should be populated by shadow run")
	}
	// Sanity: shadow run mutated the order's status to "cancelled".
	orders := got.Expected.ExpectedFinalState["orders"].(map[string]any)
	if status := orders["ORD-1001"].(map[string]any)["status"]; status != "cancelled" {
		t.Errorf("shadow run final state: want status=cancelled, got %v", status)
	}
	// And outputs lifted onto ExpectedTextFragments.
	if len(got.Expected.ExpectedTextFragments) != 1 || got.Expected.ExpectedTextFragments[0] != "cancelled" {
		t.Errorf("ExpectedTextFragments: want [cancelled], got %v", got.Expected.ExpectedTextFragments)
	}

	// --- Happy path: agent reproduces the gold trace. ---
	agent := &scriptedAgent{turns: []scriptedTurn{
		{toolName: "cancel_order", args: map[string]any{"order_id": "ORD-1001", "reason": "changed mind"}},
		{text: "Done — order ORD-1001 has been cancelled."},
	}}
	rep, err := taubench.Run(context.Background(), &taubench.Dataset{Name: "upstream-mini", Tasks: tasks}, taubench.Options{
		AgentLLM:    agent,
		Tools:       tools,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Run (happy): %v", err)
	}
	if rep.Passed != 1 {
		t.Errorf("happy: want 1 pass, got %d (reason: %s)", rep.Passed, rep.Tasks[0].Reason)
	}

	// --- Sad path: agent calls the WRONG tool. ---
	wrongAgent := &scriptedAgent{turns: []scriptedTurn{
		{toolName: "get_order", args: map[string]any{"order_id": "ORD-1001"}}, // does NOT mutate state
		{text: "I see your order."},
	}}
	rep, err = taubench.Run(context.Background(), &taubench.Dataset{Name: "upstream-mini", Tasks: tasks}, taubench.Options{
		AgentLLM:    wrongAgent,
		Tools:       tools,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Run (sad): %v", err)
	}
	if rep.Passed != 0 {
		t.Errorf("sad: want 0 pass, got %d", rep.Passed)
	}
	// The failure must point at ExpectedFinalState mismatch
	// (status was not changed to cancelled).
	if got := rep.Tasks[0].Reason; got == "" {
		t.Errorf("expected failure reason; got empty")
	}
}

// TestLoadUpstreamTasks_UnknownToolFails: a gold action referencing
// a tool that isn't in the registry must surface a loader error,
// not a silently corrupted snapshot.
func TestLoadUpstreamTasks_UnknownToolFails(t *testing.T) {
	payload := []map[string]any{
		{
			"instruction": "irrelevant",
			"actions": []map[string]any{
				{"name": "nonexistent_tool", "kwargs": map[string]any{}},
			},
		},
	}
	tasksPath := writeJSON(t, payload)
	_, err := taubench.LoadUpstreamTasks(tasksPath, retailInitialStateForUpstream(), taubench.NewRetailTools(), "retail")
	if err == nil {
		t.Fatal("expected an error for unknown tool, got nil")
	}
}

// TestExpectedTextFragments enforces case-insensitive substring
// matching. A reply that lacks ANY required fragment fails; an
// empty fragment list never fails.
func TestExpectedTextFragments(t *testing.T) {
	tasks := []taubench.Task{
		{
			ID:           "frag-1",
			Domain:       "retail",
			Instruction:  "Confirm something.",
			InitialState: retailInitialStateForUpstream(),
			Expected: taubench.ExpectedOutcome{
				ExpectedTextFragments: []string{"ORD-1001", "cancelled"},
			},
		},
	}

	// Agent reply contains both fragments → pass.
	good := &scriptedAgent{turns: []scriptedTurn{{text: "All done. Order ORD-1001 is now CANCELLED."}}}
	rep, err := taubench.Run(context.Background(), &taubench.Dataset{Tasks: tasks}, taubench.Options{
		AgentLLM:    good,
		Tools:       taubench.NewRetailTools(),
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Run (good): %v", err)
	}
	if rep.Passed != 1 {
		t.Errorf("good: want 1 pass, got %d (reason: %s)", rep.Passed, rep.Tasks[0].Reason)
	}

	// Agent reply missing one fragment → fail.
	bad := &scriptedAgent{turns: []scriptedTurn{{text: "All done. Order is cancelled."}}}
	rep, err = taubench.Run(context.Background(), &taubench.Dataset{Tasks: tasks}, taubench.Options{
		AgentLLM:    bad,
		Tools:       taubench.NewRetailTools(),
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Run (bad): %v", err)
	}
	if rep.Passed != 0 {
		t.Errorf("bad: want 0 pass (missing ORD-1001), got %d", rep.Passed)
	}
}
