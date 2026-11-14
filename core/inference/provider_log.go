package inference

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
)

// Provider-boundary logging: one record per provider round trip.
//
// These helpers live here rather than in each driver because they are identical
// apart from the provider name. They answer "which call did the endpoint
// answer/refuse", which the funnel's span does not: the span says how long it
// took and what it cost, the log record carries the provider's own request id and
// whatever it said when it refused. Failures log at Warn, successes at Debug so
// per-call correlation stays cheap enough to leave on.
//
// The protocol execution funnel calls them for every driver and every modality:
// unary calls and stream termination are adjacent to the one place that knows
// whether the provider was actually contacted. A driver may also call them
// directly when it has a richer provider id than the response metadata exposes.

// LogProviderCall records one unary provider round trip.
func LogProviderCall(
	ctx context.Context,
	provider, operation, model string,
	err error,
	requestID, responseID string,
) {
	attrs := ProviderLogAttrs(provider, operation, model, err, requestID, responseID)
	if err != nil {
		telemetry.Warn(ctx, provider+" inference request failed", attrs...)
		return
	}
	telemetry.Debug(ctx, provider+" inference request completed", attrs...)
}

// LogProviderStream records a stream opening: its failure, or its success.
func LogProviderStream(
	ctx context.Context,
	provider, operation, model string,
	err error,
	requestID string,
) {
	attrs := ProviderLogAttrs(provider, operation, model, err, requestID, "")
	if err != nil {
		telemetry.Warn(ctx, provider+" inference stream failed", attrs...)
		return
	}
	telemetry.Debug(ctx, provider+" inference stream opened", attrs...)
}

// LogProviderStreamEnd records the terminal stream event when it carries a
// provider response id for correlation.
func LogProviderStreamEnd(
	ctx context.Context,
	provider, operation, responseID string,
) {
	if responseID == "" {
		return
	}
	telemetry.Debug(ctx, provider+" inference stream completed",
		attribute.String(telemetry.AttrGenAIProviderName, provider),
		attribute.String(telemetry.AttrInferenceOperation, operation),
		attribute.String(telemetry.AttrGenAIResponseID, responseID))
}

// ProviderLogAttrs builds the attribute set shared by the helpers above.
//
// The keys follow the same namespace rule as spans (see telemetry/semconv.go):
// the facts the GenAI conventions define are gen_ai.*, the ones they do not
// define (a provider's request id, the error text) stay in flowcraft's own
// namespace. A driver that logs here and a funnel that spans the same call then
// join on the same keys.
func ProviderLogAttrs(
	provider, operation, model string,
	err error,
	requestID, responseID string,
) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 8)
	attrs = append(attrs,
		attribute.String(telemetry.AttrGenAIProviderName, provider),
		attribute.String(telemetry.AttrInferenceOperation, operation),
	)
	if model != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrGenAIRequestModel, model))
	}
	if err != nil {
		attrs = append(attrs, attribute.String(telemetry.AttrErrorMessage, err.Error()))
		var classified *Error
		if errors.As(err, &classified) {
			attrs = append(attrs,
				attribute.String(telemetry.AttrErrorKind, string(classified.Kind)))
			if classified.Detail != "" {
				attrs = append(attrs,
					attribute.String(telemetry.AttrInferenceErrorDetail, classified.Detail))
			}
			if requestID == "" {
				requestID = classified.RequestID
			}
		}
	}
	if requestID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrLLMRequestID, requestID))
	}
	if responseID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrGenAIResponseID, responseID))
	}
	return attrs
}
