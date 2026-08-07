package inference

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Runtime operations are instrumented unconditionally: spans, execution
// metrics, and token-usage counters are emitted at the Runtime funnel so
// direct callers and routed callers (whose attempts each delegate to a
// Runtime call) both produce telemetry. When no OTel SDK is configured
// the global no-op providers keep the cost at a few map lookups per
// call. Explain* methods perform no provider I/O and stay silent.
//
// Session openers (OpenTranscription, OpenRealtime) are instrumented
// around the open only; per-event session telemetry is a separate
// concern owned by the session types.

var (
	inferenceMeter = telemetry.MeterWithSuffix("inference")

	inferenceExecCount, _ = inferenceMeter.Int64Counter(
		"executions.total",
		metric.WithDescription("Total inference operation executions"))
	inferenceDuration, _ = inferenceMeter.Float64Histogram(
		"duration.seconds",
		metric.WithDescription("Inference operation duration"))
	inferenceErrorCount, _ = inferenceMeter.Int64Counter(
		"errors.total",
		metric.WithDescription("Total inference operation errors by kind"))
	inferenceInputTokens, _ = inferenceMeter.Int64Counter(
		"tokens.input",
		metric.WithDescription("Input tokens consumed by generate operations"))
	inferenceOutputTokens, _ = inferenceMeter.Int64Counter(
		"tokens.output",
		metric.WithDescription("Output tokens produced by generate operations"))
	inferenceCachedTokens, _ = inferenceMeter.Int64Counter(
		"tokens.input.cached",
		metric.WithDescription("Input tokens served from provider prompt caches"))
)

// callTelemetry carries one instrumented operation call: its span,
// start time, and the identity attributes shared by span and metrics.
type callTelemetry struct {
	span   trace.Span
	start  time.Time
	op     Operation
	model  ModelRef
	stream bool
}

// startCall opens the span and marks the start time for one operation
// call. streaming only changes the span name/suffix so dashboards can
// separate unary latency from time-to-open.
func startCall(ctx context.Context, op Operation, model ModelRef, streaming bool) (context.Context, callTelemetry) {
	name := fmt.Sprintf("inference.%s", op)
	if streaming {
		name += ".stream"
	}
	ctx, span := telemetry.Tracer().Start(ctx, name,
		trace.WithAttributes(operationAttrs(op, model, streaming)...))
	return ctx, callTelemetry{span: span, start: time.Now(), op: op, model: model, stream: streaming}
}

func operationAttrs(op Operation, model ModelRef, streaming bool) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("inference.operation", string(op)),
		attribute.String(telemetry.AttrLLMProvider, model.ID.Provider),
		attribute.String(telemetry.AttrLLMModel, model.ID.Name),
	}
	if model.Profile != "" {
		attrs = append(attrs, attribute.String("inference.profile", model.Profile))
	}
	if streaming {
		attrs = append(attrs, attribute.Bool("inference.streaming", true))
	}
	return attrs
}

func (t callTelemetry) metricAttrs(extra ...attribute.KeyValue) metric.MeasurementOption {
	base := []attribute.KeyValue{
		attribute.String("inference.operation", string(t.op)),
		attribute.String(telemetry.AttrLLMProvider, t.model.ID.Provider),
		attribute.String(telemetry.AttrLLMModel, t.model.ID.Name),
	}
	return metric.WithAttributes(append(base, extra...)...)
}

// finish closes out a call: duration and execution metrics, error
// classification, span status. err == nil records a success.
func (t callTelemetry) finish(ctx context.Context, err error) {
	inferenceDuration.Record(ctx, time.Since(t.start).Seconds(), t.metricAttrs())
	if err == nil {
		t.span.SetStatus(codes.Ok, "OK")
		t.span.End()
		inferenceExecCount.Add(ctx, 1, t.metricAttrs(attribute.String("status", "success")))
		return
	}
	kind := classifyError(err)
	t.span.SetAttributes(attribute.String("inference.error_kind", kind))
	logAttrs := []otellog.KeyValue{
		otellog.String("inference.operation", string(t.op)),
		otellog.String(telemetry.AttrLLMProvider, t.model.ID.Provider),
		otellog.String(telemetry.AttrLLMModel, t.model.ID.Name),
		otellog.String("inference.error_kind", kind),
		otellog.String(telemetry.AttrErrorMessage, err.Error()),
	}
	if requestID := requestIDOf(err); requestID != "" {
		t.span.SetAttributes(attribute.String(telemetry.AttrLLMRequestID, requestID))
		logAttrs = append(logAttrs, otellog.String(telemetry.AttrLLMRequestID, requestID))
	}
	t.span.SetStatus(codes.Error, err.Error())
	t.span.End()
	inferenceExecCount.Add(ctx, 1, t.metricAttrs(attribute.String("status", "error")))
	inferenceErrorCount.Add(ctx, 1, t.metricAttrs(attribute.String("error_kind", kind)))
	telemetry.Warn(ctx, "inference operation failed", logAttrs...)
}

// requestIDOf extracts the provider request identifier from a structured
// inference error when the provider attached one.
func requestIDOf(err error) string {
	var inferenceErr *Error
	if errors.As(err, &inferenceErr) {
		return inferenceErr.RequestID
	}
	return ""
}

// recordIDs mirrors provider-assigned request/response identifiers onto
// the call span. Empty values are left out so spans stay slim.
func (t callTelemetry) recordIDs(_ context.Context, metadata Metadata) {
	if metadata.RequestID != "" {
		t.span.SetAttributes(
			attribute.String(telemetry.AttrLLMRequestID, metadata.RequestID))
	}
	if metadata.ResponseID != "" {
		t.span.SetAttributes(
			attribute.String(telemetry.AttrLLMResponseID, metadata.ResponseID))
	}
}

// stampUsage fills the call-context envelope on a usage value about
// to cross the Runtime boundary. The envelope is Runtime-owned by
// contract, but stamping is fill-if-absent: a provider that
// explicitly reports server-side latency or a more specific model
// identity knows better than the wall clock.
func (t callTelemetry) stampUsage(usage *Usage) {
	if usage == nil {
		return
	}
	if usage.Model == (ModelRef{}) {
		usage.Model = t.model
	}
	if usage.LatencyMs == 0 {
		usage.LatencyMs = time.Since(t.start).Milliseconds()
	}
}

// recordUsage emits generate token counters and mirrors the usage onto
// the span. Zero values stay out of counters (no cache hit reported is
// not a hit-rate of zero); the cached counter only moves when the
// provider reports cache reads.
func (t callTelemetry) recordUsage(ctx context.Context, usage Usage) {
	if usage.InputTokens > 0 {
		inferenceInputTokens.Add(ctx, usage.InputTokens, t.metricAttrs())
	}
	if usage.OutputTokens > 0 {
		inferenceOutputTokens.Add(ctx, usage.OutputTokens, t.metricAttrs())
	}
	t.span.SetAttributes(
		attribute.Int64(telemetry.AttrLLMInputTokens, usage.InputTokens),
		attribute.Int64(telemetry.AttrLLMOutputTokens, usage.OutputTokens),
		attribute.Int64(telemetry.AttrLLMTotalTokens, usage.TotalTokens),
	)
	if usage.Input.CacheReadTokens != nil && *usage.Input.CacheReadTokens > 0 {
		inferenceCachedTokens.Add(ctx, *usage.Input.CacheReadTokens, t.metricAttrs())
		t.span.SetAttributes(
			attribute.Int64(telemetry.AttrLLMCachedInputTokens, *usage.Input.CacheReadTokens))
	}
}

// classifyError extracts the structured ErrorKind when the failure
// carries one; anything else is an unclassified provider-side or
// transport failure.
func classifyError(err error) string {
	var inferenceErr *Error
	if errors.As(err, &inferenceErr) {
		return string(inferenceErr.Kind)
	}
	return "unclassified"
}

// recordEmbedUsage mirrors embed usage onto the span and the input
// token counter. Item counts stay span-only: they describe request
// shape, not spend.
func (t callTelemetry) recordEmbedUsage(ctx context.Context, usage EmbedUsage) {
	if usage.InputTokens > 0 {
		inferenceInputTokens.Add(ctx, usage.InputTokens, t.metricAttrs())
	}
	t.span.SetAttributes(
		attribute.Int64(telemetry.AttrLLMInputTokens, usage.InputTokens),
		attribute.Int("inference.embed.items", usage.ItemCount),
	)
}

// recordTranscriptionUsage mirrors transcription usage onto the span.
// Audio duration is the spend dimension here; token fields are
// optional provider bonuses.
func (t callTelemetry) recordTranscriptionUsage(_ context.Context, usage TranscriptionUsage) {
	t.span.SetAttributes(
		attribute.Int64("inference.audio.duration_ms", usage.AudioDurationMillis))
	if usage.InputTokens != nil {
		t.span.SetAttributes(attribute.Int64(telemetry.AttrLLMInputTokens, *usage.InputTokens))
	}
	if usage.OutputTokens != nil {
		t.span.SetAttributes(attribute.Int64(telemetry.AttrLLMOutputTokens, *usage.OutputTokens))
	}
}

// telemetryStream wraps a GenerateStream to close out the call span
// when the stream terminates: io.EOF (clean end), a terminal error,
// or Close. Usage snapshots are cumulative, so the last one seen is
// the one recorded.
type telemetryStream struct {
	GenerateStream
	tel        callTelemetry
	ctx        context.Context
	once       sync.Once
	last       *Usage
	requestID  string
	responseID string
}

func wrapStreamTelemetry(ctx context.Context, tel callTelemetry, stream GenerateStream) GenerateStream {
	return &telemetryStream{GenerateStream: stream, tel: tel, ctx: ctx}
}

func (s *telemetryStream) Next(ctx context.Context) (GenerateStreamEvent, error) {
	event, err := s.GenerateStream.Next(ctx)
	if err == nil {
		if event.Usage != nil {
			// Stamp before retaining so the recorded snapshot and the
			// caller-visible event carry the same envelope.
			s.tel.stampUsage(event.Usage)
			s.last = event.Usage
		}
		if event.RequestID != "" {
			s.requestID = event.RequestID
		}
		if event.ResponseID != "" {
			s.responseID = event.ResponseID
		}
		return event, nil
	}
	if errors.Is(err, io.EOF) {
		s.end(nil)
		return event, err
	}
	s.end(err)
	return event, err
}

// Result returns the stream's final response with the envelope
// stamped, matching the unary Generate contract.
func (s *telemetryStream) Result() (GenerateResponse, error) {
	resp, err := s.GenerateStream.Result()
	if err == nil {
		s.tel.stampUsage(&resp.Usage)
		s.tel.recordIDs(s.ctx, resp.Metadata)
	}
	return resp, err
}

func (s *telemetryStream) Close() error {
	err := s.GenerateStream.Close()
	s.end(err)
	return err
}

// end finishes the call exactly once: a clean EOF, a terminal error, and
// Close all land here, and sync.Once keeps the ending race-free when they
// happen concurrently. Abandoned streams (caller drops the handle without
// EOF/Close) never end — the same trade-off every streaming API makes;
// callers are expected to Close.
func (s *telemetryStream) end(err error) {
	s.once.Do(func() {
		if s.last != nil {
			s.tel.recordUsage(s.ctx, *s.last)
		}
		if s.requestID != "" || s.responseID != "" {
			s.tel.recordIDs(s.ctx, Metadata{
				RequestID:  s.requestID,
				ResponseID: s.responseID,
			})
		}
		s.tel.finish(s.ctx, err)
	})
}
