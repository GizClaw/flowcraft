package a2aprotocol_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/deploy/a2aprotocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"

	"go.opentelemetry.io/otel/trace"
)

// A caller's traceparent makes this process's run a child of the caller's span.
// Without it the deployment starts a trace of its own, and the two halves of one
// user request never join.
func TestInboundTraceparentBecomesTheParent(t *testing.T) {
	recorder := telemetrytest.New(t)
	telemetrytest.Propagator(t)

	table, err := agent.NewAgent(agent.AgentOptions{
		Trace: telemetry.TraceRun,
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return &textRunner{text: "served"}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = table.Close() })
	server, err := a2aprotocol.New(a2aprotocol.Config{
		Name: "backend",
		Settings: settingsJSON(map[string]any{"card": map[string]any{
			"version": "1.0.0", "url": "https://backend.test",
		}}),
		Agent: table,
	})
	if err != nil {
		t.Fatalf("a2aprotocol.New: %v", err)
	}
	peer := httptest.NewServer(server)
	t.Cleanup(peer.Close)

	caller, callerSpan := telemetry.TracerWithSuffix("test").Start(context.Background(), "caller")
	client, err := agenta2a.New(caller, agenta2a.Config{Peer: peer.URL, SessionID: "conv-1"})
	if err != nil {
		t.Fatalf("a2a.New: %v", err)
	}
	for _, runErr := range client.Run(caller,
		message.NewTextMessage(message.RoleUser, "hi")) {
		if runErr != nil {
			t.Fatalf("run: %v", runErr)
		}
	}
	callerSpan.End()

	run := recorder.RequireNamed(t, "agent.run")
	if run.SpanContext().TraceID() != callerSpan.SpanContext().TraceID() {
		t.Fatalf("run trace %s, want the caller's %s",
			run.SpanContext().TraceID(), callerSpan.SpanContext().TraceID())
	}
	parent := run.Parent()
	if parent.SpanID() != callerSpan.SpanContext().SpanID() {
		t.Errorf("run parent = %s, want the caller's span %s",
			parent.SpanID(), callerSpan.SpanContext().SpanID())
	}
	if !parent.IsRemote() {
		t.Error("the parent is not marked remote: the header was not extracted")
	}
}

// With no caller context the deployment still runs, and the span it opens is a
// root: extraction returning nothing must not be an error path.
func TestInboundWithoutATraceparentStartsARoot(t *testing.T) {
	recorder := telemetrytest.New(t)
	telemetrytest.Propagator(t)

	table, err := agent.NewAgent(agent.AgentOptions{
		Trace: telemetry.TraceRun,
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return &textRunner{text: "served"}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = table.Close() })
	server, err := a2aprotocol.New(a2aprotocol.Config{
		Name: "backend",
		Settings: settingsJSON(map[string]any{"card": map[string]any{
			"version": "1.0.0", "url": "https://backend.test",
		}}),
		Agent: table,
	})
	if err != nil {
		t.Fatalf("a2aprotocol.New: %v", err)
	}
	peer := httptest.NewServer(server)
	t.Cleanup(peer.Close)

	client, err := agenta2a.New(context.Background(),
		agenta2a.Config{Peer: peer.URL, SessionID: "conv-1"})
	if err != nil {
		t.Fatalf("a2a.New: %v", err)
	}
	for _, runErr := range client.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		if runErr != nil {
			t.Fatalf("run: %v", runErr)
		}
	}

	run := recorder.RequireNamed(t, "agent.run")
	if run.Parent().IsValid() {
		t.Errorf("parent = %v, want a root span", run.Parent())
	}
	if run.SpanContext().TraceID() == (trace.TraceID{}) {
		t.Error("the root span has no trace id")
	}
}
