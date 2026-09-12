package inference

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// Provider transports log one event per round trip at the boundary they own:
// failures at Warn (with the provider's error and any request id it carried),
// successes at Debug so per-call correlation stays cheap. The runtime's own
// spans and metrics come from core/inference's assembly instrumentation; these
// events are the driver-side record of what the endpoint actually answered.
//
// The helpers live here rather than in each driver because they are identical
// apart from the provider name, which the caller passes.

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
		ProviderLogAttrs(provider, operation, "", nil, "", responseID)...)
}

// ProviderLogAttrs builds the standard attribute set for one provider call:
// the provider, the operation, the model, the provider's request and response
// ids (the request id is read from the error when the caller does not have it),
// and the error message. Transports that emit their own provider events extend
// this set instead of rebuilding it.
func ProviderLogAttrs(
	provider, operation, model string,
	err error,
	requestID, responseID string,
) []otellog.KeyValue {
	attrs := []otellog.KeyValue{
		otellog.String(telemetry.AttrLLMProvider, provider),
	}
	if operation != "" {
		attrs = append(attrs, otellog.String("inference.operation", operation))
	}
	if model != "" {
		attrs = append(attrs, otellog.String(telemetry.AttrLLMModel, model))
	}
	if requestID == "" {
		requestID, _ = errdefs.RequestID(err)
	}
	if requestID != "" {
		attrs = append(attrs, otellog.String(telemetry.AttrLLMRequestID, requestID))
	}
	if responseID != "" {
		attrs = append(attrs, otellog.String(telemetry.AttrLLMResponseID, responseID))
	}
	if err != nil {
		attrs = append(attrs, otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}
	return attrs
}
