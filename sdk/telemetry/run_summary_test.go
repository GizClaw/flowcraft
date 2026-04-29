package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// memSpanExporter captures every exported span in memory so test
// assertions can introspect attributes / status / timing without
// hitting an external collector.
type memSpanExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *memSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *memSpanExporter) Shutdown(context.Context) error { return nil }

func (e *memSpanExporter) snapshot() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]sdktrace.ReadOnlySpan, len(e.spans))
	copy(out, e.spans)
	return out
}

// installMemTracer replaces the global TracerProvider with one that
// pipes every span through `exp` synchronously (WithSyncer). The
// returned cleanup restores the previous provider and shuts down the
// new one. Tests that need to assert on RecordRunSummary's emitted
// attributes use this instead of InitTracer so they don't fight with
// each other over global state.
func installMemTracer(t *testing.T) *memSpanExporter {
	t.Helper()
	exp := &memSpanExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exp
}

func findAttr(attrs []attribute.KeyValue, key string) (attribute.KeyValue, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv, true
		}
	}
	return attribute.KeyValue{}, false
}

func TestRecordRunSummary_PopulatesAttributesAndStatusOk(t *testing.T) {
	exp := installMemTracer(t)

	started := time.Now().Add(-200 * time.Millisecond)
	ended := started.Add(150 * time.Millisecond)
	RecordRunSummary(context.Background(), RunSummary{
		RunID:        "r1",
		ParentRunID:  "r0",
		AgentID:      "agent-1",
		PodID:        "pod-1",
		EngineKind:   "graph",
		LLMModel:     "gpt-4o",
		InputTokens:  100,
		OutputTokens: 200,
		TotalTokens:  300,
		CostMicros:   1500,
		StartedAt:    started,
		EndedAt:      ended,
		Extra:        []attribute.KeyValue{attribute.String(AttrTenantID, "acme")},
	})

	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 exported span, got %d", len(spans))
	}
	span := spans[0]

	if span.Name() != "engine.run.summary" {
		t.Errorf("name = %q, want engine.run.summary", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("status code = %v, want Ok", span.Status().Code)
	}

	want := map[string]any{
		AttrRunID:           "r1",
		AttrParentRunID:     "r0",
		AttrAgentID:         "agent-1",
		AttrPodID:           "pod-1",
		AttrEngineKind:      "graph",
		AttrRunStatus:       "ok",
		AttrLLMModel:        "gpt-4o",
		AttrLLMInputTokens:  int64(100),
		AttrLLMOutputTokens: int64(200),
		AttrLLMTotalTokens:  int64(300),
		AttrLLMCostMicros:   int64(1500),
		AttrLLMLatencyMs:    int64(150),
		AttrTenantID:        "acme",
	}
	for k, expectedRaw := range want {
		kv, ok := findAttr(span.Attributes(), k)
		if !ok {
			t.Errorf("missing attribute %q", k)
			continue
		}
		switch expected := expectedRaw.(type) {
		case string:
			if got := kv.Value.AsString(); got != expected {
				t.Errorf("attribute %q = %q, want %q", k, got, expected)
			}
		case int64:
			if got := kv.Value.AsInt64(); got != expected {
				t.Errorf("attribute %q = %d, want %d", k, got, expected)
			}
		default:
			t.Fatalf("unhandled expected type for %q: %T", k, expectedRaw)
		}
	}
}

func TestRecordRunSummary_DefaultsStatusToOkWhenEmpty(t *testing.T) {
	exp := installMemTracer(t)
	RecordRunSummary(context.Background(), RunSummary{RunID: "r1"})
	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	kv, ok := findAttr(spans[0].Attributes(), AttrRunStatus)
	if !ok || kv.Value.AsString() != "ok" {
		t.Fatalf("AttrRunStatus = %v / present=%v, want \"ok\"/true", kv.Value.AsString(), ok)
	}
}

func TestRecordRunSummary_ErrorMarksFailedAndRecordsError(t *testing.T) {
	exp := installMemTracer(t)
	cause := errors.New("boom")
	RecordRunSummary(context.Background(), RunSummary{
		RunID: "r1",
		Err:   cause,
	})

	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	span := spans[0]

	if span.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", span.Status().Code)
	}
	if span.Status().Description != cause.Error() {
		t.Errorf("status description = %q, want %q", span.Status().Description, cause.Error())
	}

	kv, ok := findAttr(span.Attributes(), AttrRunStatus)
	if !ok || kv.Value.AsString() != "failed" {
		t.Fatalf("AttrRunStatus = %v / present=%v, want \"failed\"/true", kv.Value.AsString(), ok)
	}

	// Recorded error should appear as an event on the span.
	events := span.Events()
	foundExceptionEvent := false
	for _, ev := range events {
		if ev.Name == "exception" {
			foundExceptionEvent = true
			break
		}
	}
	if !foundExceptionEvent {
		t.Errorf("expected an \"exception\" event on the span, events = %+v", events)
	}
}

func TestRecordRunSummary_ExplicitStatusOverridesErrorDefault(t *testing.T) {
	exp := installMemTracer(t)
	RecordRunSummary(context.Background(), RunSummary{
		RunID:  "r1",
		Status: "interrupted",
		Err:    errors.New("user_input"),
	})
	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	kv, ok := findAttr(spans[0].Attributes(), AttrRunStatus)
	if !ok || kv.Value.AsString() != "interrupted" {
		t.Fatalf("AttrRunStatus = %v, want \"interrupted\"", kv.Value.AsString())
	}
}

func TestRecordRunSummary_OmitsZeroValueOptionalAttributes(t *testing.T) {
	exp := installMemTracer(t)
	RecordRunSummary(context.Background(), RunSummary{
		RunID: "r1",
	})
	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	attrs := spans[0].Attributes()

	mustAbsent := []string{
		AttrParentRunID,
		AttrAgentID,
		AttrPodID,
		AttrEngineKind,
		AttrLLMModel,
		AttrLLMInputTokens,
		AttrLLMOutputTokens,
		AttrLLMTotalTokens,
		AttrLLMCostMicros,
	}
	for _, k := range mustAbsent {
		if _, ok := findAttr(attrs, k); ok {
			t.Errorf("attribute %q should be omitted when empty/zero", k)
		}
	}

	// RunID and Status should still be present.
	if _, ok := findAttr(attrs, AttrRunID); !ok {
		t.Errorf("AttrRunID should always be set when non-empty")
	}
	if _, ok := findAttr(attrs, AttrRunStatus); !ok {
		t.Errorf("AttrRunStatus should always be set")
	}
}

func TestRecordRunSummary_HandlesNilContextAndZeroValue(t *testing.T) {
	// MUST NOT panic on either of these.
	exp := installMemTracer(t)

	//nolint:staticcheck // intentionally passing nil ctx — we test the helper tolerates it.
	RecordRunSummary(nil, RunSummary{})

	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	// With a fully zero RunSummary the only attribute we expect is Status.
	kv, ok := findAttr(spans[0].Attributes(), AttrRunStatus)
	if !ok || kv.Value.AsString() != "ok" {
		t.Fatalf("zero-value RunSummary should still emit Status=ok, got present=%v val=%v", ok, kv.Value.AsString())
	}
}

func TestRecordRunSummary_LatencyOnlyEmittedWhenPositive(t *testing.T) {
	exp := installMemTracer(t)

	// EndedAt before StartedAt — latency MUST be omitted (negative
	// duration is meaningless).
	RecordRunSummary(context.Background(), RunSummary{
		RunID:     "r1",
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(-1 * time.Second),
	})
	spans := exp.snapshot()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if _, ok := findAttr(spans[0].Attributes(), AttrLLMLatencyMs); ok {
		t.Errorf("AttrLLMLatencyMs should be omitted when EndedAt <= StartedAt")
	}
}
