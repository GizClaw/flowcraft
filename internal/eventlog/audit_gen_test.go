package eventlog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderAuditSummary_TaskSubmitted(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"card_id":         "card-123",
		"target_agent_id": "agent-7",
	})
	env := Envelope{Type: EventTypeTaskSubmitted, Payload: payload}
	got := RenderAuditSummary(env)
	want := "task card-123 submitted for agent agent-7"
	if got != want {
		t.Fatalf("RenderAuditSummary mismatch:\n  got=%q\n want=%q", got, want)
	}
}

func TestRenderAuditSummary_RealmCreated(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"realm_id": "rt-abc"})
	env := Envelope{Type: EventTypeRealmCreated, Payload: payload}
	if got, want := RenderAuditSummary(env), "realm rt-abc created"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderAuditSummary_MissingFieldFallsBack(t *testing.T) {
	env := Envelope{Type: EventTypeTaskSubmitted, Payload: []byte(`{}`)}
	got := RenderAuditSummary(env)
	if !strings.Contains(got, "<missing>") {
		t.Fatalf("expected <missing> placeholder, got %q", got)
	}
}

func TestRenderAuditSummary_UnknownTypeReturnsEmpty(t *testing.T) {
	env := Envelope{Type: "unknown.type", Payload: []byte(`{"x":1}`)}
	if got := RenderAuditSummary(env); got != "" {
		t.Fatalf("expected empty for unknown type, got %q", got)
	}
}

func TestAuditRequiredEventTypes_NotEmpty(t *testing.T) {
	if len(AuditRequiredEventTypes) == 0 {
		t.Fatal("AuditRequiredEventTypes should not be empty in R4")
	}
}
