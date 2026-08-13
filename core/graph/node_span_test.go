package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestNodeSpanErrorRecordsRequestID(t *testing.T) {
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	reg := NewRegistry()
	if err := RegisterType(reg, "failing", NodeType[struct{}]{
		Handler: func(_ ExecutionContext, _ *agent.Board, _ struct{}) error {
			return errdefs.WithRequestID(
				errdefs.Validation(errors.New("boom")), "req-node-1")
		},
	}); err != nil {
		t.Fatalf("register failing: %v", err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "n",
		Nodes: []NodeDefinition{{ID: "n", Type: "failing"}},
	}, reg)

	if _, err := g.Execute(
		context.Background(), testRun(), agent.NoopHost{}, agent.NewBoard(),
	); err == nil {
		t.Fatal("expected node failure")
	}

	var found bool
	for _, span := range rec.Ended() {
		for _, kv := range span.Attributes() {
			if kv.Key != telemetry.AttrLLMRequestID {
				continue
			}
			found = true
			if got := kv.Value.AsString(); got != "req-node-1" {
				t.Fatalf("llm.request.id = %q, want req-node-1", got)
			}
		}
	}
	if !found {
		t.Fatal("node span did not record llm.request.id")
	}
}
