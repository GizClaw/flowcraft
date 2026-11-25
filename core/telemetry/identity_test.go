package telemetry_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/telemetry"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
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

// TestAttrsForRendersNothingForANonRecordingSpan keeps the cheap path honest: a span that
// drops every attribute it is given gets nothing rendered for it, whether that is a no-op
// span (no provider installed, or the trace level filtered the span) or one the sampler
// dropped.
func TestAttrsForRendersNothingForANonRecordingSpan(t *testing.T) {
	ctx := identity.Attach(context.Background(), identity.Run{
		Tenant: "tenant-1", Agent: "assistant", SessionID: "session-1",
	})

	if got := telemetry.AttrsFor(tracenoop.Span{}, ctx); got != nil {
		t.Fatalf("AttrsFor(noop span) = %v, want nil", got)
	}

	droppedProvider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	t.Cleanup(func() { _ = droppedProvider.Shutdown(context.Background()) })
	_, dropped := droppedProvider.Tracer("test").Start(ctx, "dropped")
	defer dropped.End()
	if got := telemetry.AttrsFor(dropped, ctx); got != nil {
		t.Fatalf("AttrsFor(sampled-out span) = %v, want nil", got)
	}
}

// TestAttrsForRendersIdentityOnARecordingSpan pins that the guard changes nothing on the
// path that does record: the same keys in the same order Attrs renders.
func TestAttrsForRendersIdentityOnARecordingSpan(t *testing.T) {
	run := identity.Run{
		Tenant: "tenant-1", Agent: "assistant", Authority: "authority-1",
		SessionID: "session-1", Bind: "bind-1", TaskID: "task-1", Ordinal: 2,
		ChainID: "chain-1", Depth: 1,
	}
	ctx := identity.Attach(context.Background(), run)

	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	_, span := provider.Tracer("test").Start(ctx, "recording")
	defer span.End()
	if !span.IsRecording() {
		t.Fatal("the sampled span is not recording; the test cannot exercise the rendering path")
	}

	got := telemetry.AttrsFor(span, ctx)
	want := telemetry.Attrs(ctx)
	if len(want) == 0 {
		t.Fatal("Attrs rendered no identity attributes")
	}
	if len(got) != len(want) {
		t.Fatalf("AttrsFor = %d attributes, Attrs = %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("attribute %d = %v, want %v", i, got[i], want[i])
		}
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
