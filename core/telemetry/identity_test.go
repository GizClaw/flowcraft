package telemetry_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// TestAttrsRendersNothingOutsideASession keeps an identity-free span honest: no
// attributes rather than empty ones, because an empty string matches every filter in
// a dashboard.
func TestAttrsRendersNothingOutsideASession(t *testing.T) {
	if got := telemetry.Attrs(context.Background()); got != nil {
		t.Fatalf("Attrs = %v, want nil", got)
	}
	if got := telemetry.AttrsOf(identity.Run{}); got != nil {
		t.Fatalf("AttrsOf(zero) = %v, want nil", got)
	}
}

// TestAttrsKeepsTheRunOrder pins the rendering: same keys, same order as
// identity.Run.Pairs, so two spans in one trace differ by value and never by order.
func TestAttrsKeepsTheRunOrder(t *testing.T) {
	run := identity.Run{
		Tenant: "tenant-1", Agent: "assistant", Authority: "authority-1",
		SessionID: "session-1", Bind: "bind-1", TaskID: "task-1", Ordinal: 2,
		ChainID: "chain-1", Depth: 1,
	}
	ctx := identity.Attach(context.Background(), run)

	fromContext := telemetry.Attrs(ctx)
	fromValue := telemetry.AttrsOf(run)
	if len(fromContext) != len(fromValue) {
		t.Fatalf("Attrs = %d keys, AttrsOf = %d", len(fromContext), len(fromValue))
	}

	pairs := run.Pairs()
	for i, pair := range pairs {
		if string(fromContext[i].Key) != pair[0] || fromContext[i].Value.AsString() != pair[1] {
			t.Fatalf("attribute %d = %v, want %v", i, fromContext[i], pair)
		}
	}
}
