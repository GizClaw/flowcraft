package agent

import (
	"testing"
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

// TestRunInfoFromAttributes_RunIDArgWinsOverAttribute asserts the
// caller-supplied runID overrides any attribute copy. ExecutionContext
// exposes RunID as a dedicated field separate from Attributes; the
// helper trusts the dedicated field so a stale Attributes copy
// (e.g. cloned across attempts) cannot poison observers.
func TestRunInfoFromAttributes_RunIDArgWinsOverAttribute(t *testing.T) {
	attrs := map[string]string{
		attrAgentID: "a",
		attrRunID:   "stale-run-id",
	}
	got := RunInfoFromAttributes("authoritative-run-id", attrs)
	if got.RunID != "authoritative-run-id" {
		t.Fatalf("RunID arg should win over attribute copy: got %q", got.RunID)
	}
}
