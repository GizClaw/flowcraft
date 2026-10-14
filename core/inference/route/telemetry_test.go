package route

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
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
	ctx, span := startRouteSpan(context.Background(), model.OperationGenerate)
	recordRoute(ctx, span, model.OperationGenerate, Trace{},
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

func TestRouteFailureLogCarriesRedactedCause(t *testing.T) {
	var out bytes.Buffer
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(
			telemetry.NewPlainTextExporter(&out))))
	previous := logglobal.GetLoggerProvider()
	logglobal.SetLoggerProvider(provider)
	t.Cleanup(func() {
		logglobal.SetLoggerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	// A completed response the request contract rejects: the chain bottoms
	// out in a flowcraft-generated check message, so the warn line carries
	// it as error.cause next to the stable Detail label.
	rejected := inference.GenerateResponse{
		Message:      message.Message{Role: message.RoleAssistant},
		FinishReason: inference.FinishCompleted,
	}
	err := inference.NewError(
		inference.ProviderResponseUnusable, model.OperationGenerate, "",
		rejected.ValidateFor(routeRequest()))
	err.Detail = "stream.finish.validation.empty_content"
	ctx, span := startRouteSpan(context.Background(), model.OperationGenerate)
	recordRoute(ctx, span, model.OperationGenerate, Trace{}, inference.Metadata{}, err)

	got := out.String()
	for _, want := range []string{
		"inference route failed",
		`error.message="provider_response_unusable during generate: stream.finish.validation.empty_content"`,
		`error.cause="generate response content is required for finish reason`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("route failure log missing %q:\n%s", want, got)
		}
	}

	// A decode-style failure keeps its cause out of the log: driver and
	// transport text is diagnostic data, not redacted output.
	out.Reset()
	decodeErr := inference.NewError(
		inference.InvalidProviderResponse, model.OperationGenerate, "",
		errors.New("decode boom"))
	decodeErr.Detail = "stream.decode"
	ctx, span = startRouteSpan(context.Background(), model.OperationGenerate)
	recordRoute(ctx, span, model.OperationGenerate, Trace{}, inference.Metadata{}, decodeErr)

	got = out.String()
	if strings.Contains(got, "error.cause=") {
		t.Fatalf("decode failure must not carry error.cause:\n%s", got)
	}
	if !strings.Contains(got, `error.message="invalid_provider_response during generate: stream.decode"`) {
		t.Fatalf("decode failure log missing error.message:\n%s", got)
	}
}

func TestRouteStreamRecordsIDsOnCompletion(t *testing.T) {
	rec := installTestTracer(t)
	ctx, span := startRouteSpan(context.Background(), model.OperationGenerate)
	wrapped := wrapRouteStream(ctx, span, model.OperationGenerate, Trace{}, &fakeRouteStream{
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
	ctx, span := startRouteSpan(context.Background(), model.OperationGenerate)
	wrapped := wrapRouteStream(ctx, span, model.OperationGenerate, Trace{}, &fakeRouteStream{
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
