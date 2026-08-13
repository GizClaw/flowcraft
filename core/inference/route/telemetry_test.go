package route

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func installTestTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return rec
}

func spanAttributeValue(span sdktrace.ReadOnlySpan, key attribute.Key) (string, bool) {
	for _, kv := range span.Attributes() {
		if kv.Key == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

type fakeRouteStream struct {
	events    []inference.GenerateStreamEvent
	next      int
	err       error
	result    inference.GenerateResponse
	resultErr error
}

func (s *fakeRouteStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	if s.next < len(s.events) {
		event := s.events[s.next]
		s.next++
		return event, nil
	}
	if s.err != nil {
		return inference.GenerateStreamEvent{}, s.err
	}
	return inference.GenerateStreamEvent{}, io.EOF
}

func (s *fakeRouteStream) Result() (inference.GenerateResponse, error) {
	return s.result, s.resultErr
}

func (*fakeRouteStream) Close() error { return nil }

func TestRecordRouteErrorRecordsRequestID(t *testing.T) {
	rec := installTestTracer(t)
	ctx, span := startRouteSpan(context.Background(), inference.OperationGenerate)
	recordRoute(ctx, span, inference.OperationGenerate, Trace{},
		inference.Metadata{},
		errdefs.WithRequestID(errdefs.Validation(errors.New("boom")), "req-err-1"))

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got, ok := spanAttributeValue(spans[0], telemetry.AttrLLMRequestID); !ok || got != "req-err-1" {
		t.Fatalf("llm.request.id = %q/%v, want req-err-1", got, ok)
	}
	if _, ok := spanAttributeValue(spans[0], telemetry.AttrLLMResponseID); ok {
		t.Fatal("llm.response.id should be absent on error")
	}
}

func TestRouteStreamRecordsIDsOnCompletion(t *testing.T) {
	rec := installTestTracer(t)
	ctx, span := startRouteSpan(context.Background(), inference.OperationGenerate)
	wrapped := wrapRouteStream(ctx, span, inference.OperationGenerate, Trace{}, &fakeRouteStream{
		events: []inference.GenerateStreamEvent{{
			PartIndex: 0,
			Delta:     inference.TextPartDelta{Text: "hi"},
		}},
		result: inference.GenerateResponse{
			Metadata: inference.Metadata{RequestID: "req-1", ResponseID: "resp-1"},
		},
	})

	for {
		_, err := wrapped.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if _, err := wrapped.Result(); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if err := wrapped.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got, ok := spanAttributeValue(spans[0], telemetry.AttrLLMRequestID); !ok || got != "req-1" {
		t.Fatalf("llm.request.id = %q/%v, want req-1", got, ok)
	}
	if got, ok := spanAttributeValue(spans[0], telemetry.AttrLLMResponseID); !ok || got != "resp-1" {
		t.Fatalf("llm.response.id = %q/%v, want resp-1", got, ok)
	}
}

func TestRouteStreamRecordsRequestIDOnFailure(t *testing.T) {
	rec := installTestTracer(t)
	ctx, span := startRouteSpan(context.Background(), inference.OperationGenerate)
	wrapped := wrapRouteStream(ctx, span, inference.OperationGenerate, Trace{}, &fakeRouteStream{
		err: errdefs.WithRequestID(errdefs.Validation(errors.New("boom")), "req-err-2"),
	})

	if _, err := wrapped.Next(context.Background()); err == nil {
		t.Fatal("Next unexpectedly succeeded")
	}
	_ = wrapped.Close()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got, ok := spanAttributeValue(spans[0], telemetry.AttrLLMRequestID); !ok || got != "req-err-2" {
		t.Fatalf("llm.request.id = %q/%v, want req-err-2", got, ok)
	}
	if _, ok := spanAttributeValue(spans[0], telemetry.AttrLLMResponseID); ok {
		t.Fatal("llm.response.id should be absent on failure")
	}
}
