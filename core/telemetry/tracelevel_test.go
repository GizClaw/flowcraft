package telemetry_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TestParseTraceLevel is the document's half: the four spellings, and an error rather than a
// silent default for anything else. A deployment that asked for a level this build does not
// know should hear about it at load time.
func TestParseTraceLevel(t *testing.T) {
	for name, want := range map[string]telemetry.TraceLevel{
		"":     telemetry.TraceNone,
		"none": telemetry.TraceNone,
		"run":  telemetry.TraceRun,
		"call": telemetry.TraceCall,
		"node": telemetry.TraceNode,
		// Case and surrounding space are the document author's, not a difference.
		" CALL ": telemetry.TraceCall,
	} {
		got, err := telemetry.ParseTraceLevel(name)
		if err != nil || got != want {
			t.Fatalf("ParseTraceLevel(%q) = %v, %v", name, got, err)
		}
	}
	if _, err := telemetry.ParseTraceLevel("everything"); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	if telemetry.TraceCall.String() != "call" {
		t.Fatalf("String = %q", telemetry.TraceCall.String())
	}
	if got := telemetry.TraceLevel(9).String(); got != "TraceLevel(9)" {
		t.Fatalf("unknown String = %q", got)
	}
}

// TestRecordsWithoutALevel states the degradation rule: a context nobody narrowed records
// everything. A command-line tool and a test call a producer directly, and neither should have
// to install a level to see a span.
func TestRecordsWithoutALevel(t *testing.T) {
	if !telemetry.Records(context.Background(), telemetry.TraceNode) {
		t.Fatal("an empty context stopped recording")
	}
	if _, ok := telemetry.TraceFrom(context.Background()); ok {
		t.Fatal("an empty context reported a level")
	}
}

// TestStartFollowsTheFloor is the mechanism every producer relies on: the floor decides, the
// attached level answers, and a suppressed span is still safe to end and annotate.
func TestStartFollowsTheFloor(t *testing.T) {
	for _, tc := range []struct {
		level telemetry.TraceLevel
		floor telemetry.TraceLevel
		want  bool
	}{
		{telemetry.TraceNone, telemetry.TraceNone, true},
		{telemetry.TraceNone, telemetry.TraceCall, false},
		{telemetry.TraceRun, telemetry.TraceCall, false},
		{telemetry.TraceCall, telemetry.TraceCall, true},
		{telemetry.TraceNode, telemetry.TraceCall, true},
		{telemetry.TraceCall, telemetry.TraceNode, false},
	} {
		t.Run(tc.level.String()+"_"+tc.floor.String(), func(t *testing.T) {
			recorder := telemetrytest.New(t)
			ctx := telemetry.WithTrace(context.Background(), tc.level)
			_, span := telemetry.Start(ctx,
				telemetry.TracerWithSuffix("test"), "test.span", tc.floor,
				trace.WithAttributes(attribute.String("k", "v")))
			// A suppressed span must still tolerate the whole span API.
			span.AddEvent("test.event")
			span.SetAttributes(attribute.String("k2", "v2"))
			span.End()

			got := len(recorder.Named("test.span"))
			if want := boolToInt(tc.want); got != want {
				t.Fatalf("spans recorded = %d, want %d", got, want)
			}
		})
	}
}

// TestStartLeavesTheContextAloneWhenSuppressed is what keeps a suppressed span invisible: a
// caller that inspects the context must not find a non-recording span it never asked for, and
// a child span opened later is filtered by the same level, not by a fake parent.
func TestStartLeavesTheContextAloneWhenSuppressed(t *testing.T) {
	ctx := telemetry.WithTrace(context.Background(), telemetry.TraceNone)
	out, _ := telemetry.Start(ctx,
		telemetry.TracerWithSuffix("test"), "test.span", telemetry.TraceCall)
	if out != ctx {
		t.Fatal("Start replaced the context of a suppressed span")
	}
	if trace.SpanContextFromContext(out).IsValid() {
		t.Fatal("a suppressed span was attached to the context")
	}
}

// TestWithTraceReplacesTheLevel keeps the attach rule honest: the innermost answer wins, which
// is what a delegated run under a narrower level relies on.
func TestWithTraceReplacesTheLevel(t *testing.T) {
	ctx := telemetry.WithTrace(context.Background(), telemetry.TraceNode)
	ctx = telemetry.WithTrace(ctx, telemetry.TraceNone)
	level, ok := telemetry.TraceFrom(ctx)
	if !ok || level != telemetry.TraceNone {
		t.Fatalf("level = %v, %v; want none", level, ok)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
