package metrics

import (
	"testing"
	"time"
)

func TestTurnMetrics_Fields(t *testing.T) {
	m := TurnMetrics{
		SessionID:   "sess-1",
		TurnID:      "turn-1",
		RunID:       "run-1",
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
		EndToEnd:    100 * time.Millisecond,
		Interrupted: true,
	}
	if m.SessionID != "sess-1" {
		t.Fatalf("expected sess-1, got %s", m.SessionID)
	}
	if !m.Interrupted {
		t.Fatal("expected Interrupted to be true")
	}
}

func TestHookFunc(t *testing.T) {
	var called bool
	h := HookFunc(func(m TurnMetrics) {
		called = true
		if m.TurnID != "t1" {
			t.Fatalf("expected t1, got %s", m.TurnID)
		}
	})

	h.OnTurnMetrics(TurnMetrics{TurnID: "t1"})
	if !called {
		t.Fatal("expected hook to be called")
	}
}

func TestHookInterface(t *testing.T) {
	var h Hook = HookFunc(func(TurnMetrics) {})
	h.OnTurnMetrics(TurnMetrics{})
}
