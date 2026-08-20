package inference

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Assembly operations are instrumented at the funnel: every provider
// round trip emits execution/duration/token metrics and mirrors the
// provider request/response ids and token usage onto a span. When the
// caller already runs inside a span (a graph node span, a route span,
// a script node span) that span is reused so identifiers and usage
// land on the caller's span without nesting a second inference span
// per provider round trip; otherwise a dedicated
// "inference.<operation>" span is created and the call owns ending it.
// Explain* methods perform no provider I/O and stay silent.
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

// inferenceCall carries one instrumented operation call: its span,
// start time, and the identity attributes shared by span and metrics.
type inferenceCall struct {
	ctx   context.Context
	span  trace.Span
	start time.Time
	op    Operation
	model ModelRef
	reuse bool
}

// startInferenceCall opens the instrumentation for one operation call.
// It reuses the caller's active span when present (see package doc);
// otherwise it starts a dedicated inference.<operation> span. The call
// owner closes the instrumentation via finish.
func startInferenceCall(
	ctx context.Context,
	op Operation,
	ref ModelRef,
) (context.Context, inferenceCall) {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, inferenceCall{
			ctx:   ctx,
			span:  trace.SpanFromContext(ctx),
			start: time.Now(),
			op:    op,
			model: ref,
			reuse: true,
		}
	}
	ctx, span := telemetry.Tracer().Start(ctx, "inference."+string(op),
		trace.WithAttributes(
			attribute.String("inference.operation", string(op)),
			attribute.String(telemetry.AttrLLMProvider, ref.ID.Provider),
			attribute.String(telemetry.AttrLLMModel, ref.ID.Name),
		))
	return ctx, inferenceCall{
		ctx:   ctx,
		span:  span,
		start: time.Now(),
		op:    op,
		model: ref,
	}
}

func (c inferenceCall) metricAttrs(extra ...attribute.KeyValue) metric.MeasurementOption {
	base := []attribute.KeyValue{
		attribute.String("inference.operation", string(c.op)),
		attribute.String(telemetry.AttrLLMProvider, c.model.ID.Provider),
		attribute.String(telemetry.AttrLLMModel, c.model.ID.Name),
	}
	return metric.WithAttributes(append(base, extra...)...)
}

// stampUsage fills the call-context envelope on a usage value about to
// cross the Assembly boundary. The envelope is runtime-owned by
// contract, but stamping is fill-if-absent: a provider that explicitly
// reports server-side latency or a more specific model identity knows
// better than the wall clock.
func (c inferenceCall) stampUsage(usage *Usage) {
	if usage == nil {
		return
	}
	if usage.Model == (ModelRef{}) {
		usage.Model = c.model
	}
	if usage.LatencyMs == 0 {
		usage.LatencyMs = time.Since(c.start).Milliseconds()
	}
}

// finish closes out one operation call: duration and execution
// metrics, token counters, span attributes, and (for self-created
// spans) status and lifetime. err == nil records a success. Reused
// spans only gain attributes; the owner controls status and lifetime.
func (c inferenceCall) finish(metadata Metadata, usage Usage, err error) {
	recordLLMAttrs(c.span, metadata, usage, err)
	recordUsageMetrics(c.ctx, usage, c.metricAttrs())
	inferenceDuration.Record(c.ctx, time.Since(c.start).Seconds(), c.metricAttrs())
	if err == nil {
		inferenceExecCount.Add(c.ctx, 1, c.metricAttrs(attribute.String("status", "success")))
		if !c.reuse {
			c.span.SetStatus(codes.Ok, "OK")
			c.span.End()
		}
		return
	}
	kind := classifyInferenceError(err)
	c.span.SetAttributes(attribute.String("inference.error_kind", kind))
	inferenceExecCount.Add(c.ctx, 1, c.metricAttrs(attribute.String("status", "error")))
	inferenceErrorCount.Add(c.ctx, 1, c.metricAttrs(attribute.String("error_kind", kind)))
	if !c.reuse {
		c.span.RecordError(err)
		c.span.SetStatus(codes.Error, err.Error())
		c.span.End()
		return
	}
}

// recordEmbedUsage mirrors embed usage onto the span and the input
// token counter. Item counts stay span-only: they describe request
// shape, not spend.
func (c inferenceCall) recordEmbedUsage(ctx context.Context, usage EmbedUsage) {
	if usage.InputTokens > 0 {
		inferenceInputTokens.Add(ctx, usage.InputTokens, c.metricAttrs())
	}
	c.span.SetAttributes(
		attribute.Int64(telemetry.AttrLLMInputTokens, usage.InputTokens),
		attribute.Int("inference.embed.items", usage.ItemCount),
	)
}

// recordLLMAttrs mirrors request/response ids and usage dimensions
// onto span (attrs only; metrics are emitted by the call owner).
func recordLLMAttrs(span trace.Span, metadata Metadata, usage Usage, err error) {
	if span == nil {
		return
	}
	if metadata.RequestID != "" {
		span.SetAttributes(attribute.String(telemetry.AttrLLMRequestID, metadata.RequestID))
	}
	if metadata.ResponseID != "" {
		span.SetAttributes(attribute.String(telemetry.AttrLLMResponseID, metadata.ResponseID))
	}
	recordInferenceUsage(span, usage)
	if err != nil {
		if requestID, ok := errdefs.RequestID(err); ok {
			span.SetAttributes(attribute.String(telemetry.AttrLLMRequestID, requestID))
		}
	}
}

// RecordLLMTelemetry mirrors the provider request / response
// identifiers and token usage of a completed inference call onto the
// span active in ctx. It is a no-op when ctx carries no span or the
// values are empty; err is consulted for a request id wrapped via
// errdefs.WithRequestID when metadata carries none. Host code that
// performs its own inference bookkeeping (e.g. the graph inference
// node) uses it to surface ids and usage on its own span after a call.
func RecordLLMTelemetry(ctx context.Context, metadata Metadata, usage Usage, err error) {
	span := trace.SpanFromContext(ctx)
	if span == nil || !span.SpanContext().IsValid() {
		return
	}
	recordLLMAttrs(span, metadata, usage, err)
}

// recordInferenceUsage mirrors an inference.Usage envelope onto span as
// llm.* attributes. Zero-valued optional dimensions are omitted, so
// spans stay slim on the common path and dashboards can rely on
// presence rather than zero-sentinel values.
func recordInferenceUsage(span trace.Span, usage Usage) {
	if span == nil {
		return
	}
	if usage.Model.ID.Provider != "" {
		span.SetAttributes(attribute.String(telemetry.AttrLLMProvider, usage.Model.ID.Provider))
	}
	if usage.Model.ID.Name != "" {
		span.SetAttributes(attribute.String(telemetry.AttrLLMModel, usage.Model.ID.Name))
	}
	if usage.InputTokens > 0 {
		span.SetAttributes(attribute.Int64(telemetry.AttrLLMInputTokens, usage.InputTokens))
	}
	if usage.OutputTokens > 0 {
		span.SetAttributes(attribute.Int64(telemetry.AttrLLMOutputTokens, usage.OutputTokens))
	}
	if usage.TotalTokens > 0 {
		span.SetAttributes(attribute.Int64(telemetry.AttrLLMTotalTokens, usage.TotalTokens))
	}
	if usage.Input.CacheReadTokens != nil && *usage.Input.CacheReadTokens > 0 {
		span.SetAttributes(attribute.Int64(
			telemetry.AttrLLMCachedInputTokens, *usage.Input.CacheReadTokens))
	}
	if usage.LatencyMs > 0 {
		span.SetAttributes(attribute.Int64(telemetry.AttrLLMLatencyMs, usage.LatencyMs))
	}
	if usage.AudioDurationMillis != nil && *usage.AudioDurationMillis > 0 {
		span.SetAttributes(attribute.Int64(
			"inference.audio.duration_ms", *usage.AudioDurationMillis))
	}
	if usage.Billing != nil && usage.Billing.Cost != nil {
		if micros, ok := costMicros(*usage.Billing.Cost); ok {
			span.SetAttributes(attribute.Int64(telemetry.AttrLLMCostMicros, micros))
		}
	}
}

// recordUsageMetrics emits the token counters for a generate /
// transcription usage envelope. Zero values stay out of counters (no
// cache hit reported is not a hit-rate of zero); the cached counter
// only moves when the provider reports cache reads.
func recordUsageMetrics(
	ctx context.Context,
	usage Usage,
	opts metric.MeasurementOption,
) {
	if usage.InputTokens > 0 {
		inferenceInputTokens.Add(ctx, usage.InputTokens, opts)
	}
	if usage.OutputTokens > 0 {
		inferenceOutputTokens.Add(ctx, usage.OutputTokens, opts)
	}
	if usage.Input.CacheReadTokens != nil && *usage.Input.CacheReadTokens > 0 {
		inferenceCachedTokens.Add(ctx, *usage.Input.CacheReadTokens, opts)
	}
}

// classifyInferenceError extracts the structured ErrorKind when the
// failure carries one; anything else is an unclassified provider-side
// or transport failure.
func classifyInferenceError(err error) string {
	var inferenceErr *Error
	if errors.As(err, &inferenceErr) {
		return string(inferenceErr.Kind)
	}
	return "unclassified"
}

// costMicros converts a Money (Units / 10^Scale) into the integer
// micro-unit representation the AttrLLMCostMicros attribute uses. It
// reports ok=false when converting would overflow or a sub-micro
// remainder floors to zero.
func costMicros(money Money) (int64, bool) {
	if money.Scale > 18 {
		return 0, false
	}
	if money.Scale > 6 {
		micros := money.Units / pow10(money.Scale-6)
		return micros, micros > 0
	}
	factor := pow10(6 - money.Scale)
	if money.Units > math.MaxInt64/factor {
		return 0, false
	}
	return money.Units * factor, true
}

// pow10 returns exactly 10^n for n in [0, 18], matching Money.Scale's
// validated range.
func pow10(n uint8) int64 {
	var value int64 = 1
	for i := uint8(0); i < n; i++ {
		value *= 10
	}
	return value
}

// telemetryGenerateStream wraps a GenerateStream so the provider
// request / response identifiers — only known once the stream reaches
// its terminal result — and the final usage snapshot are recorded when
// the call completes. finish runs once: the first terminal path
// (Result, mid-stream error, or Close) owns the recorded outcome.
type telemetryGenerateStream struct {
	inner GenerateStream
	tel   inferenceCall
	once  sync.Once
}

func (s *telemetryGenerateStream) finish(metadata Metadata, usage Usage, err error) {
	s.once.Do(func() {
		s.tel.finish(metadata, usage, err)
	})
}

func (s *telemetryGenerateStream) Next(ctx context.Context) (GenerateStreamEvent, error) {
	event, err := s.inner.Next(ctx)
	if err == nil && event.Usage != nil {
		// Stamp before returning so the recorded snapshot and the
		// caller-visible event carry the same envelope.
		s.tel.stampUsage(event.Usage)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		s.finish(Metadata{}, Usage{}, err)
	}
	return event, err
}

func (s *telemetryGenerateStream) Result() (GenerateResponse, error) {
	response, err := s.inner.Result()
	s.tel.stampUsage(&response.Usage)
	s.finish(response.Metadata, response.Usage, err)
	return response, err
}

func (s *telemetryGenerateStream) Close() error {
	err := s.inner.Close()
	s.finish(Metadata{}, Usage{}, errdefs.Validationf(
		"inference: generate stream closed before completion"))
	return err
}

// telemetryTranscriptionSession mirrors telemetryGenerateStream for
// duplex transcription sessions.
type telemetryTranscriptionSession struct {
	inner TranscriptionSession
	tel   inferenceCall
	once  sync.Once
}

func (s *telemetryTranscriptionSession) finish(metadata Metadata, usage Usage, err error) {
	s.once.Do(func() {
		s.tel.finish(metadata, usage, err)
	})
}

func (s *telemetryTranscriptionSession) Send(ctx context.Context, chunk media.AudioChunk) error {
	return s.inner.Send(ctx, chunk)
}

func (s *telemetryTranscriptionSession) Next(ctx context.Context) (TranscriptionSessionEvent, error) {
	event, err := s.inner.Next(ctx)
	if err != nil && !errors.Is(err, io.EOF) {
		s.finish(Metadata{}, Usage{}, err)
	}
	return event, err
}

func (s *telemetryTranscriptionSession) Result() (TranscriptionResponse, error) {
	response, err := s.inner.Result()
	s.tel.stampUsage(&response.Usage)
	s.finish(response.Metadata, response.Usage, err)
	return response, err
}

func (s *telemetryTranscriptionSession) Interrupt() error {
	err := s.inner.Interrupt()
	s.finish(Metadata{}, Usage{}, err)
	return err
}

func (s *telemetryTranscriptionSession) Close() error {
	err := s.inner.Close()
	s.finish(Metadata{}, Usage{}, errdefs.Validationf(
		"inference: transcription session closed before completion"))
	return err
}
