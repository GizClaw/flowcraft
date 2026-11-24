package agent_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/outcome"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

// TestTurnMetricsAreEmitted pins the series an operator sums: one turn counted, its
// duration recorded, and its tokens split by direction. The labels are the low-cardinality
// ones — the session and task ids stay on the span.
func TestTurnMetricsAreEmitted(t *testing.T) {
	recorder := telemetrytest.Meters(t)

	driver := agenttest.NewScripted(
		agent.UsageEvent{Usage: outcome.Usage{InputTokens: 10, OutputTokens: 4}},
		agenttest.Completed(),
	).OpenDriver()
	if _, err := collect(agent.RunTurn(sessionCtx(t), request(nil), driver)); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	got := recorder.Collect(t)
	turns, ok := got.SumWhere("agent.turns", map[string]string{
		identity.AttrAgentName: "helper",
		"agent.state":          "completed",
	})
	if !ok || turns != 1 {
		t.Fatalf("turns = %v (ok=%v), want 1; emitted: %v", turns, ok, got.Names())
	}
	if count, ok := got.HistogramCount("agent.turn.duration"); !ok || count != 1 {
		t.Fatalf("duration observations = %d (ok=%v)", count, ok)
	}
	input, ok := got.SumWhere("agent.tokens", map[string]string{"kind": "input"})
	if !ok || input != 10 {
		t.Fatalf("input tokens = %v (ok=%v)", input, ok)
	}
	output, ok := got.SumWhere("agent.tokens", map[string]string{"kind": "output"})
	if !ok || output != 4 {
		t.Fatalf("output tokens = %v (ok=%v)", output, ok)
	}
}

// TestFailedTurnCountsAsFailed keeps the counter honest about the outcome: a turn that
// ended on an error is not counted as completed, whichever state the driver last reported.
func TestFailedTurnCountsAsFailed(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	driver := agenttest.NewScripted(agent.Text{Text: "x"}).FailAt(context.DeadlineExceeded).OpenDriver()
	if _, err := collect(agent.RunTurn(sessionCtx(t), request(nil), driver)); err == nil {
		t.Fatal("the turn did not fail")
	}

	got := recorder.Collect(t)
	if value, ok := got.SumWhere("agent.turns", map[string]string{"agent.state": "failed"}); !ok || value != 1 {
		t.Fatalf("failed turns = %v (ok=%v); emitted: %v", value, ok, got.Names())
	}
	if value, ok := got.SumWhere("agent.turn.errors", map[string]string{"error.kind": "timeout"}); !ok || value != 1 {
		t.Fatalf("timeout errors = %v (ok=%v)", value, ok)
	}
}
