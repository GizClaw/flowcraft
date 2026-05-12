package agent

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

// TestRunInfoFromAttributes_RoundTrip asserts the
// mergeAttributes write side and RunInfoFromAttributes read side
// agree on the wire format. If anyone changes one half without the
// other, this test turns red — closing the historical "scriptnode
// reads ec.RunID and silently loses the other three identity
// fields" gap (contract-audit #12) at the contract level rather
// than the call-site level.
func TestRunInfoFromAttributes_RoundTrip(t *testing.T) {
	const (
		agentID   = "researcher"
		runID     = "run-77"
		taskID    = "task-42"
		contextID = "ctx-9"
	)
	attrs := mergeAttributes(nil,
		Request{TaskID: taskID, ContextID: contextID},
		Agent{ID: agentID}, runID)

	got := RunInfoFromAttributes(runID, attrs)

	want := RunInfo{
		AgentID:   agentID,
		RunID:     runID,
		TaskID:    taskID,
		ContextID: contextID,
	}
	if got != want {
		t.Fatalf("round-trip mismatch:\n  got  %+v\n  want %+v", got, want)
	}
}

// TestRunInfoFromAttributes_MissingKeys documents that absent
// keys yield empty strings rather than errors — matching Run's
// "promote when non-empty" write policy.
func TestRunInfoFromAttributes_MissingKeys(t *testing.T) {
	got := RunInfoFromAttributes("run-1", nil)
	if got.RunID != "run-1" {
		t.Errorf("RunID: got %q want %q", got.RunID, "run-1")
	}
	if got.AgentID != "" || got.TaskID != "" || got.ContextID != "" {
		t.Errorf("missing keys should yield empty strings, got %+v", got)
	}

	got = RunInfoFromAttributes("run-2", map[string]string{"unrelated": "x"})
	if got.AgentID != "" {
		t.Errorf("unrelated key should not bleed into AgentID: got %+v", got)
	}
}

// TestMergeAttributes_WriteSideUsesTelemetryDotKeys pins the wire
// format on the write side. Migrating off snake_case "agent_id" /
// "run_id" / "task_id" / "context_id" was the precondition for the
// executor reading actor_id from engine.Run.Attributes (contract-
// audit #15): the executor — across module boundaries — has no
// access to agent-private constants, so the keys MUST live in
// sdk/telemetry where every layer can reference them.
//
// If anyone reverts to snake_case in mergeAttributes, this test
// turns red and the executor's actorIDFor lookup silently breaks.
func TestMergeAttributes_WriteSideUsesTelemetryDotKeys(t *testing.T) {
	attrs := mergeAttributes(nil,
		Request{TaskID: "task-1", ContextID: "conv-1"},
		Agent{ID: "agent-x"}, "run-1")

	wantKeys := map[string]string{
		telemetry.AttrAgentID:        "agent-x",
		telemetry.AttrRunID:          "run-1",
		telemetry.AttrTaskID:         "task-1",
		telemetry.AttrConversationID: "conv-1",
	}
	for k, want := range wantKeys {
		if got := attrs[k]; got != want {
			t.Errorf("Attributes[%q] = %q, want %q", k, got, want)
		}
	}

	// Snake_case legacy keys MUST NOT leak — they would dilute the
	// canonical-key dashboard joins and re-introduce the dual-format
	// trap the migration was designed to remove.
	for _, legacy := range []string{"agent_id", "run_id", "task_id", "context_id"} {
		if _, ok := attrs[legacy]; ok {
			t.Errorf("legacy snake_case key %q must not be set after migration; got %v", legacy, attrs)
		}
	}
}

// TestRunInfoFromAttributes_RunIDArgWinsOverAttribute asserts the
// caller-supplied runID overrides any attribute copy. ExecutionContext
// exposes RunID as a dedicated field separate from Attributes; the
// helper trusts the dedicated field so a stale Attributes copy
// (e.g. cloned across attempts) cannot poison observers.
func TestRunInfoFromAttributes_RunIDArgWinsOverAttribute(t *testing.T) {
	attrs := map[string]string{
		telemetry.AttrAgentID: "a",
		telemetry.AttrRunID:   "stale-run-id",
	}
	got := RunInfoFromAttributes("authoritative-run-id", attrs)
	if got.RunID != "authoritative-run-id" {
		t.Fatalf("RunID arg should win over attribute copy: got %q", got.RunID)
	}
}
