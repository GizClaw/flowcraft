package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

// A finished run is reported with the ids a trace exporter joins on.
func TestOnRunReportsTheRun(t *testing.T) {
	recorder := telemetrytest.New(t)
	factory, _ := newFakeFactory(false)
	var records []agent.RunRecord
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: factory,
		Trace:     telemetry.TraceRun,
		RunContext: func(ctx context.Context, sessionID string) context.Context {
			return runctx.WithRun(ctx, runctx.Run{SessionID: sessionID, TenantID: "acme", Manifest: "sha256:m"})
		},
		OnRun: func(record agent.RunRecord) { records = append(records, record) },
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	for range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want one", len(records))
	}
	record := records[0]
	if record.SessionID != "c1" || record.TaskID == "" {
		t.Errorf("record = %+v, want the session and task", record)
	}
	if record.State != agent.StateCompleted {
		t.Errorf("state = %s, want completed", record.State)
	}
	if record.Manifest != "sha256:m" {
		t.Errorf("manifest = %q", record.Manifest)
	}
	if record.TenantID != "acme" {
		t.Errorf("tenant = %q", record.TenantID)
	}
	if record.TraceID == "" || record.SpanID == "" {
		t.Errorf("trace ids = %q/%q, want the run span", record.TraceID, record.SpanID)
	}
	// The record carries both ends of the run: a latency without an anchor
	// cannot be joined to anything outside the process.
	if record.StartedAt.IsZero() {
		t.Error("the record has no start time")
	}
	if record.StartedAt.After(time.Now()) {
		t.Errorf("started at %v, which is in the future", record.StartedAt)
	}
	if record.StartedAt.Add(time.Duration(record.LatencyMs) * time.Millisecond).Before(record.StartedAt) {
		t.Error("the run ended before it started")
	}
	if span := recorder.RequireNamed(t, "agent.run"); span.Name() != "agent.run" {
		t.Errorf("span = %q", span.Name())
	}
}
