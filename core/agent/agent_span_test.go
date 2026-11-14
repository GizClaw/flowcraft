package agent_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

// A run span carries the identity the run context attached: without it a trace
// is a set of unrelated LLM calls.
func TestRunSpanCarriesTheRunIdentity(t *testing.T) {
	recorder := telemetrytest.New(t)
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: factory,
		Trace:     telemetry.TraceRun,
		RunContext: func(ctx context.Context, sessionID string) context.Context {
			return runctx.WithRun(ctx, runctx.Run{SessionID: sessionID, AgentName: "writer"})
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	span := recorder.RequireNamed(t, "agent.run")
	attrs := make(map[attribute.Key]attribute.Value, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	if got := attrs[telemetry.AttrSessionID].AsString(); got != "c1" {
		t.Errorf("session.id = %q, want c1", got)
	}
	if got := attrs[telemetry.AttrAgentName].AsString(); got != "writer" {
		t.Errorf("agent.name = %q, want writer", got)
	}
}

// Tracing off emits nothing, so an unconfigured run pays no span cost.
func TestRunSpanIsAbsentWhenTracingIsOff(t *testing.T) {
	recorder := telemetrytest.New(t)
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	if spans := recorder.Spans(); len(spans) != 0 {
		t.Fatalf("spans = %d, want none", len(spans))
	}
}
