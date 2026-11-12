package delegation_test

import (
	"context"
	"iter"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"

	"go.opentelemetry.io/otel/trace"
)

// asyncTrace starts an async delegation under a parent span and returns the span
// context the child's run was handed, plus the job's recorded traceparent.
//
// The fake session stands in for the host handle, so what it sees in its context
// is exactly what agent.Agent.Run would turn into the child's run span.
func asyncTrace(t *testing.T) (parent, handed trace.SpanContext) {
	t.Helper()
	telemetrytest.New(t)
	telemetrytest.Propagator(t)

	sessions, child := newFake()
	child.run = func(f *fakeSession, ctx context.Context) iter.Seq2[agent.Event, error] {
		handed = trace.SpanContextFromContext(ctx)
		return func(yield func(agent.Event, error) bool) {
			f.setState(agent.StateWorking)
			if !yield(agent.Status{State: agent.StateWorking}, nil) {
				return
			}
			f.setState(agent.StateCompleted)
			yield(agent.Status{State: agent.StateCompleted}, nil)
		}
	}
	service, err := delegation.New(sessions, asyncPolicy())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	ctx, span := telemetry.TracerWithSuffix("test").Start(
		parentCtx("call-1"), "parent")
	parent = span.SpanContext()

	outcome, err := service.Delegate(ctx, delegation.Request{
		Mode: delegation.ModeAsync, Target: "worker", Input: "do it",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if outcome.Status != delegation.StatusAccepted {
		t.Fatalf("status = %s, want accepted", outcome.Status)
	}
	waitStatus(t, service, ctx, outcome.ID, delegation.StatusCompleted)
	span.End()
	return parent, handed
}

// An async child runs after the call that started it has returned, so the
// parent's context is gone by then: the job carries the traceparent and the
// child's run joins the trace it came from.
func TestAsyncChildJoinsTheParentTrace(t *testing.T) {
	parent, handed := asyncTrace(t)
	if !handed.IsValid() {
		t.Fatal("the child ran without a span context")
	}
	if handed.TraceID() != parent.TraceID() {
		t.Fatalf("child trace %s, want the parent's %s", handed.TraceID(), parent.TraceID())
	}
	if handed.SpanID() != parent.SpanID() {
		t.Errorf("child parent span = %s, want %s", handed.SpanID(), parent.SpanID())
	}
	// The parent's span ended before the child started, so the relation is a
	// remote parent rather than a live one.
	if !handed.IsRemote() {
		t.Error("the parent is not marked remote: the job's traceparent was not extracted")
	}
}

// Without a parent span nothing is attached: a delegation started outside a
// traced turn must not resurrect an identity it never had.
func TestAsyncChildWithoutAParentSpanStaysARoot(t *testing.T) {
	telemetrytest.New(t)
	telemetrytest.Propagator(t)

	sessions, child := newFake()
	var handed trace.SpanContext
	child.run = func(f *fakeSession, ctx context.Context) iter.Seq2[agent.Event, error] {
		handed = trace.SpanContextFromContext(ctx)
		return func(yield func(agent.Event, error) bool) {
			f.setState(agent.StateCompleted)
			yield(agent.Status{State: agent.StateCompleted}, nil)
		}
	}
	service, err := delegation.New(sessions, asyncPolicy())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	ctx := parentCtx("call-1")
	outcome, err := service.Delegate(ctx, delegation.Request{
		Mode: delegation.ModeAsync, Target: "worker", Input: "do it",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	waitStatus(t, service, ctx, outcome.ID, delegation.StatusCompleted)
	if handed.IsValid() {
		t.Errorf("the child inherited span %s from an untraced parent", handed.SpanID())
	}
}

// The sync path needs no job: the child runs inside the caller's call, so the
// context crossing the boundary is the caller's own. Pinned because it is the
// one line an async refactor is most likely to copy wrongly.
func TestSyncChildInheritsTheCallerContext(t *testing.T) {
	telemetrytest.New(t)
	telemetrytest.Propagator(t)

	sessions, child := newFake()
	var handed trace.SpanContext
	child.run = func(f *fakeSession, ctx context.Context) iter.Seq2[agent.Event, error] {
		handed = trace.SpanContextFromContext(ctx)
		return func(yield func(agent.Event, error) bool) {
			f.setState(agent.StateCompleted)
			yield(agent.Status{State: agent.StateCompleted}, nil)
		}
	}
	service, err := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	ctx, span := telemetry.TracerWithSuffix("test").Start(parentCtx("call-1"), "parent")
	parent := span.SpanContext()
	if _, err := service.Delegate(ctx, delegation.Request{Target: "worker", Input: "do it"}); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	span.End()

	if handed.SpanID() != parent.SpanID() {
		t.Fatalf("child inherited span %s, want the caller's %s", handed.SpanID(), parent.SpanID())
	}
	if handed.IsRemote() {
		t.Error("an in-process parent is not remote")
	}
}
