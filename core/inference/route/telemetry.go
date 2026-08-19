package route

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Routed calls get one route-level span per logical request. The span
// records the selector's decision, every attempt as a span event, and
// the final executed target — the "did we fall back, how often, why"
// story that per-attempt Runtime spans (each attempt delegates to a
// Runtime call, so they nest below this span) cannot tell on their own.

var (
	routeMeter = telemetry.MeterWithSuffix("inference.route")

	routeExecCount, _ = routeMeter.Int64Counter(
		"executions.total",
		metric.WithDescription("Total routed inference operations"))
	routeFallbackCount, _ = routeMeter.Int64Counter(
		"fallbacks.total",
		metric.WithDescription("Routed operations that fell back at least once"))
	routeRetryCount, _ = routeMeter.Int64Counter(
		"retries.total",
		metric.WithDescription("Same-target retry attempts"))
	routeCircuitOpens, _ = routeMeter.Int64Counter(
		"circuit.opens",
		metric.WithDescription("Circuit transitions to open"))
	routeCircuitSkips, _ = routeMeter.Int64Counter(
		"circuit.skips",
		metric.WithDescription("Attempts skipped because the circuit was open"))
	routeCircuitProbes, _ = routeMeter.Int64Counter(
		"circuit.half_open_probes",
		metric.WithDescription("Half-open circuit probes"))
)

func startRouteSpan(ctx context.Context, operation inference.Operation) (context.Context, trace.Span) {
	return telemetry.Tracer().Start(ctx, "inference.route."+string(operation),
		trace.WithAttributes(attribute.String("inference.operation", string(operation))))
}

// recordRoute closes a routed call: attempt events, route shape
// attributes, and final status. routeTrace is the value returned to
// the caller, so a failed routing still records the attempts that ran.
func recordRoute(
	ctx context.Context,
	span trace.Span,
	operation inference.Operation,
	routeTrace Trace,
	metadata inference.Metadata,
	err error,
) {
	opAttr := attribute.String("inference.operation", string(operation))
	selected := routeTrace.Decision.Selected
	retries := 0
	for _, attempt := range routeTrace.Attempts {
		if attempt.Trigger == AttemptTriggerRetry {
			retries++
		}
	}
	span.SetAttributes(
		attribute.String("route.selected.provider", selected.ID.Provider),
		attribute.String("route.selected.model", selected.ID.Name),
		attribute.Int("route.attempts", len(routeTrace.Attempts)),
		attribute.Int("route.fallbacks", len(routeTrace.Fallbacks)),
		attribute.Int("route.retries", retries),
	)
	for _, attempt := range routeTrace.Attempts {
		attrs := []attribute.KeyValue{
			attribute.String("target.provider", attempt.Target.ID.Provider),
			attribute.String("target.model", attempt.Target.ID.Name),
			attribute.String("phase", string(attempt.Phase)),
			attribute.String("trigger", string(attempt.Trigger)),
			attribute.String("outcome", string(attempt.Outcome)),
		}
		if attempt.ErrorKind != "" {
			attrs = append(attrs, attribute.String("error_kind", string(attempt.ErrorKind)))
		}
		if attempt.Number > 0 {
			attrs = append(attrs, attribute.Int("attempt", attempt.Number))
		}
		if attempt.BackoffMillis > 0 {
			attrs = append(attrs, attribute.Int64("backoff_ms", attempt.BackoffMillis))
		}
		if attempt.Circuit != "" {
			attrs = append(attrs, attribute.String("circuit", attempt.Circuit))
		}
		if attempt.CircuitTransition != "" {
			attrs = append(
				attrs,
				attribute.String("circuit_transition", attempt.CircuitTransition),
			)
		}
		if attempt.WireAttempts > 0 {
			attrs = append(attrs, attribute.Int("wire_attempts", attempt.WireAttempts))
		}
		span.AddEvent("route.attempt", trace.WithAttributes(attrs...))
		if attempt.Trigger == AttemptTriggerRetry {
			routeRetryCount.Add(ctx, 1, metric.WithAttributes(
				opAttr,
				attribute.String(telemetry.AttrLLMProvider, attempt.Target.ID.Provider),
				attribute.String(telemetry.AttrLLMModel, attempt.Target.ID.Name),
				attribute.String("error_kind", string(attempt.ErrorKind)),
			))
		}
	}
	if err != nil {
		routeExecCount.Add(ctx, 1, metric.WithAttributes(opAttr, attribute.String("status", "error")))
		if requestID, ok := errdefs.RequestID(err); ok {
			span.SetAttributes(
				attribute.String(telemetry.AttrLLMRequestID, requestID))
		}
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return
	}
	executed := routeTrace.Executed
	span.SetAttributes(
		attribute.String("route.executed.provider", executed.ID.Provider),
		attribute.String("route.executed.model", executed.ID.Name),
	)
	if metadata.RequestID != "" {
		span.SetAttributes(
			attribute.String(telemetry.AttrLLMRequestID, metadata.RequestID))
	}
	if metadata.ResponseID != "" {
		span.SetAttributes(
			attribute.String(telemetry.AttrLLMResponseID, metadata.ResponseID))
	}
	if len(routeTrace.Fallbacks) > 0 {
		routeFallbackCount.Add(ctx, 1, metric.WithAttributes(opAttr))
	}
	routeExecCount.Add(ctx, 1, metric.WithAttributes(opAttr, attribute.String("status", "success")))
	span.SetStatus(codes.Ok, "OK")
	span.End()
}

// routeGenerateStream defers the route span close from stream-open
// time to stream completion. The provider-issued request / response
// identifiers ride the terminal finish event, so they are only known
// once the stream has been drained to its result.
type routeGenerateStream struct {
	inner      inference.GenerateStream
	ctx        context.Context
	span       trace.Span
	operation  inference.Operation
	routeTrace Trace
	once       sync.Once
}

func wrapRouteStream(
	ctx context.Context,
	span trace.Span,
	operation inference.Operation,
	routeTrace Trace,
	inner inference.GenerateStream,
) inference.GenerateStream {
	return &routeGenerateStream{
		inner:      inner,
		ctx:        ctx,
		span:       span,
		operation:  operation,
		routeTrace: routeTrace,
	}
}

func (s *routeGenerateStream) finish(metadata inference.Metadata, err error) {
	s.once.Do(func() {
		recordRoute(s.ctx, s.span, s.operation, s.routeTrace, metadata, err)
	})
}

func (s *routeGenerateStream) Next(ctx context.Context) (inference.GenerateStreamEvent, error) {
	event, err := s.inner.Next(ctx)
	if err != nil && !errors.Is(err, io.EOF) {
		s.finish(inference.Metadata{}, err)
	}
	return event, err
}

func (s *routeGenerateStream) Result() (inference.GenerateResponse, error) {
	response, err := s.inner.Result()
	s.finish(response.Metadata, err)
	return response, err
}

func (s *routeGenerateStream) Close() error {
	err := s.inner.Close()
	s.finish(inference.Metadata{}, errdefs.Validationf(
		"inference route: stream closed before completion"))
	return err
}

// routeTranscriptionSession defers the route span close from session-open
// time to session completion, mirroring routeGenerateStream. A session may
// also end through Interrupt (barge-in), which reports the interruption to
// the span.
type routeTranscriptionSession struct {
	inner      inference.TranscriptionSession
	ctx        context.Context
	span       trace.Span
	operation  inference.Operation
	routeTrace Trace
	once       sync.Once
}

func wrapRouteTranscriptionSession(
	ctx context.Context,
	span trace.Span,
	operation inference.Operation,
	routeTrace Trace,
	inner inference.TranscriptionSession,
) inference.TranscriptionSession {
	return &routeTranscriptionSession{
		inner:      inner,
		ctx:        ctx,
		span:       span,
		operation:  operation,
		routeTrace: routeTrace,
	}
}

func (s *routeTranscriptionSession) finish(
	metadata inference.Metadata,
	err error,
) {
	s.once.Do(func() {
		recordRoute(s.ctx, s.span, s.operation, s.routeTrace, metadata, err)
	})
}

func (s *routeTranscriptionSession) Send(
	ctx context.Context,
	chunk media.AudioChunk,
) error {
	return s.inner.Send(ctx, chunk)
}

func (s *routeTranscriptionSession) Next(
	ctx context.Context,
) (inference.TranscriptionSessionEvent, error) {
	event, err := s.inner.Next(ctx)
	if err != nil && !errors.Is(err, io.EOF) {
		s.finish(inference.Metadata{}, err)
	}
	return event, err
}

func (s *routeTranscriptionSession) Result() (inference.TranscriptionResponse, error) {
	response, err := s.inner.Result()
	s.finish(response.Metadata, err)
	return response, err
}

func (s *routeTranscriptionSession) Interrupt() error {
	err := s.inner.Interrupt()
	s.finish(inference.Metadata{}, err)
	return err
}

func (s *routeTranscriptionSession) Close() error {
	err := s.inner.Close()
	s.finish(inference.Metadata{}, errdefs.Validationf(
		"inference route: transcription session closed before completion"))
	return err
}
