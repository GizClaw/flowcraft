package agent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
)

// TestAgentCard_JSONKeysMatchA2A pins the AgentCard JSON encoding to
// the keys an A2A reader expects. Renaming a field here without
// updating the JSON tag (or vice versa) breaks A2A interop, so this
// test is a guardrail rather than coverage.
//
// Reference: https://agent2agent.info/docs/concepts/agentcard/
func TestAgentCard_JSONKeysMatchA2A(t *testing.T) {
	card := agent.AgentCard{
		Name:               "demo",
		Description:        "demo agent",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Capabilities: agent.AgentCapabilities{
			Streaming:              true,
			PushNotifications:      true,
			StateTransitionHistory: true,
		},
		Skills: []agent.Skill{{
			ID:          "s1",
			Name:        "skill 1",
			Description: "does stuff",
			Tags:        []string{"demo"},
			Examples:    []string{"do thing"},
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain"},
		}},
	}

	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := []string{
		`"name"`,
		`"description"`,
		`"skills"`,
		`"defaultInputModes"`,
		`"defaultOutputModes"`,
		`"capabilities"`,
		`"streaming"`,
		`"pushNotifications"`,
		`"stateTransitionHistory"`,
		`"id"`,
		`"tags"`,
		`"examples"`,
		`"inputModes"`,
		`"outputModes"`,
	}
	got := string(raw)
	for _, key := range want {
		if !strings.Contains(got, key) {
			t.Errorf("AgentCard JSON missing A2A-required key %s; got: %s", key, got)
		}
	}

	forbidden := []string{
		`"input_modes"`,
		`"output_modes"`,
		`"push_notification"`,
		`"state_transition"`,
		`"InputModes"`,
		`"PushNotification"`,
	}
	for _, key := range forbidden {
		if strings.Contains(got, key) {
			t.Errorf("AgentCard JSON contains non-A2A key %s; got: %s", key, got)
		}
	}
}

// TestRequest_JSONKeysMatchA2A pins the Request JSON encoding to A2A's
// MessageSendParams schema (camelCase: taskId, contextId,
// configuration → acceptedOutputModes).
func TestRequest_JSONKeysMatchA2A(t *testing.T) {
	req := agent.Request{
		TaskID:    "t1",
		ContextID: "c1",
		RunID:     "r1",
		Config: &agent.RequestConfig{
			AcceptedOutputModes: []string{"text/plain"},
		},
	}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := []string{
		`"taskId"`,
		`"contextId"`,
		`"configuration"`,
		`"acceptedOutputModes"`,
	}
	got := string(raw)
	for _, key := range want {
		if !strings.Contains(got, key) {
			t.Errorf("Request JSON missing A2A-required key %s; got: %s", key, got)
		}
	}

	forbidden := []string{
		`"task_id"`,
		`"context_id"`,
		`"run_id"`,
		`"config"`,
		`"accepted_output_modes"`,
	}
	for _, key := range forbidden {
		if strings.Contains(got, key) {
			t.Errorf("Request JSON contains non-A2A key %s; got: %s", key, got)
		}
	}
}

// TestResult_JSONKeys pins Result's JSON tags. Result mostly mirrors
// the A2A task-status shape (taskId, status, …) but adds agent-only
// policy fields (committed) that have no A2A counterpart and SHOULD
// stay agent-internal.
func TestResult_JSONKeys(t *testing.T) {
	res := agent.Result{
		TaskID:    "t1",
		RunID:     "r1",
		Status:    agent.StatusCompleted,
		Committed: true,
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	want := []string{`"taskId"`, `"runId"`, `"status"`, `"committed"`}
	for _, key := range want {
		if !strings.Contains(got, key) {
			t.Errorf("Result JSON missing key %s; got: %s", key, got)
		}
	}

	// Result no longer carries a Usage field — token aggregation is
	// the caller-supplied engine.Host's job (see agent.WithEngineHost
	// for the rationale). The JSON must not regress to including it.
	forbidden := []string{
		`"task_id"`, `"run_id"`,
		`"err"`,
		`"last_board"`, `"lastBoard"`,
		`"usage"`,
	}
	for _, key := range forbidden {
		if strings.Contains(got, key) {
			t.Errorf("Result JSON contains forbidden key %s; got: %s", key, got)
		}
	}
}

// TestAgent_JSONOmitsNonSerialisableHooks ensures the Observers /
// Deciders fields stay JSON-skipped — they hold runtime state and
// would not round-trip through serialisation cleanly.
func TestAgent_JSONOmitsNonSerialisableHooks(t *testing.T) {
	a := agent.Agent{
		ID:        "a1",
		Card:      agent.AgentCard{Name: "demo"},
		Tools:     []string{"web.search"},
		Observers: []agent.Observer{agent.BaseObserver{}},
		Deciders:  []agent.Decider{agent.BaseDecider{}},
	}

	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	want := []string{`"id"`, `"card"`, `"tools"`}
	for _, key := range want {
		if !strings.Contains(got, key) {
			t.Errorf("Agent JSON missing key %s; got: %s", key, got)
		}
	}
	forbidden := []string{`"observers"`, `"Observers"`, `"deciders"`, `"Deciders"`}
	for _, key := range forbidden {
		if strings.Contains(got, key) {
			t.Errorf("Agent JSON contains forbidden key %s; got: %s", key, got)
		}
	}
}

// TestStatusValues pins the Status string constants — they cross
// serialisation boundaries and must stay stable.
func TestStatusValues(t *testing.T) {
	pairs := []struct {
		s    agent.Status
		want string
	}{
		{agent.StatusCompleted, "completed"},
		{agent.StatusInterrupted, "interrupted"},
		{agent.StatusCanceled, "canceled"},
		{agent.StatusFailed, "failed"},
		{agent.StatusAborted, "aborted"},
	}
	for _, p := range pairs {
		if string(p.s) != p.want {
			t.Errorf("Status %q has value %q, want %q", p.s, string(p.s), p.want)
		}
	}
}
