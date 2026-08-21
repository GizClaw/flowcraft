package openai

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// providerID is the stable telemetry token for this provider, matching
// the inference.ModelID.Provider values in the catalog.
const providerID = "openai"

// logInferenceCall records one provider round trip at the transport
// boundary. Failures are Warn (with error.message and the provider
// request id when the error chain carries one); successes are Debug so
// per-call correlation stays cheap. responseID is only populated where
// the wire response exposes an identifier.
func logInferenceCall(
	ctx context.Context,
	op, model string,
	err error,
	requestID, responseID string,
) {
	attrs := inferenceAttrs(op, model, err, requestID, responseID)
	if err != nil {
		telemetry.Warn(ctx, "openai inference request failed", attrs...)
		return
	}
	telemetry.Debug(ctx, "openai inference request completed", attrs...)
}

// logInferenceStream records stream open and terminal failure. The
// terminal response id is logged separately via logInferenceStreamEnd
// once the adapter assembles it.
func logInferenceStream(
	ctx context.Context,
	op, model string,
	err error,
	requestID string,
) {
	attrs := inferenceAttrs(op, model, err, requestID, "")
	if err != nil {
		telemetry.Warn(ctx, "openai inference stream failed", attrs...)
		return
	}
	telemetry.Debug(ctx, "openai inference stream opened", attrs...)
}

// logInferenceStreamEnd records the terminal stream event carrying the
// provider response id for correlation.
func logInferenceStreamEnd(ctx context.Context, op, responseID string) {
	if responseID == "" {
		return
	}
	telemetry.Debug(ctx, "openai inference stream completed",
		inferenceAttrs(op, "", nil, "", responseID)...)
}

func inferenceAttrs(
	op, model string,
	err error,
	requestID, responseID string,
) []otellog.KeyValue {
	attrs := []otellog.KeyValue{
		otellog.String(telemetry.AttrLLMProvider, providerID),
	}
	if op != "" {
		attrs = append(attrs, otellog.String("inference.operation", op))
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
