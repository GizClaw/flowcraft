package session

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Validate is what a caller past a point of no return uses instead of applying
// and hoping: it has to answer with the same verdicts UpdateTunables would,
// without touching a manager.
func TestTunablesValidate(t *testing.T) {
	valid := Tunables{
		IdleTimeout:         time.Minute,
		SinkBuffer:          8,
		SpeculativeEvents:   4,
		SpeculativeBytes:    4096,
		DeliveryConcurrency: 2,
		MaxSessions:         16,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid) = %v, want nil", err)
	}

	tests := []struct {
		name   string
		mutate func(*Tunables)
	}{
		{"zero idle timeout", func(t *Tunables) { t.IdleTimeout = 0 }},
		{"negative idle timeout", func(t *Tunables) { t.IdleTimeout = -time.Second }},
		{"zero sink buffer", func(t *Tunables) { t.SinkBuffer = 0 }},
		{"negative sink buffer", func(t *Tunables) { t.SinkBuffer = -1 }},
		{"zero speculative events", func(t *Tunables) { t.SpeculativeEvents = 0 }},
		{"zero speculative bytes", func(t *Tunables) { t.SpeculativeBytes = 0 }},
		{"zero delivery concurrency", func(t *Tunables) { t.DeliveryConcurrency = 0 }},
		{"zero max sessions", func(t *Tunables) { t.MaxSessions = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tunables := valid
			test.mutate(&tunables)
			if err := tunables.Validate(); !errdefs.IsValidation(err) {
				t.Fatalf("Validate = %v, want a Validation error", err)
			}
		})
	}
}

// A valid set of tunables reaches the manager, and a rejected one leaves the
// running limits alone — Validate and UpdateTunables answer identically.
func TestUpdateTunablesKeepsLimitsOnRejection(t *testing.T) {
	manager, _ := newTestManager(t, time.Minute)
	before := manager.maxSessions

	err := manager.UpdateTunables(Tunables{
		IdleTimeout:         time.Minute,
		SinkBuffer:          8,
		SpeculativeEvents:   4,
		SpeculativeBytes:    4096,
		DeliveryConcurrency: 2,
		MaxSessions:         -3,
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("UpdateTunables = %v, want a Validation error", err)
	}
	if manager.maxSessions != before {
		t.Fatalf("max sessions = %d after a rejected update, want %d", manager.maxSessions, before)
	}
}
