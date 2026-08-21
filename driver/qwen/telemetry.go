package qwen

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// providerID is the stable telemetry token for this provider, matching
// the inference.ModelID.Provider values in the catalog.
const providerID = "qwen"

func logInferenceCall(
	ctx context.Context,
	op, model string,
	err error,
	requestID, responseID string,
) {
	attrs := inferenceAttrs(op, model, err, requestID, responseID)
	if err != nil {
		telemetry.Warn(ctx, "qwen inference request failed", attrs...)
		return
	}
	telemetry.Debug(ctx, "qwen inference request completed", attrs...)
}

func logInferenceStream(
	ctx context.Context,
	op, model string,
	err error,
	requestID string,
) {
	attrs := inferenceAttrs(op, model, err, requestID, "")
	if err != nil {
		telemetry.Warn(ctx, "qwen inference stream failed", attrs...)
		return
	}
	telemetry.Debug(ctx, "qwen inference stream opened", attrs...)
}

// logInferenceStreamEnd records the terminal stream event. DashScope
// streams identify the call by request_id rather than a response id.
func logInferenceStreamEnd(ctx context.Context, op, requestID string) {
	if requestID == "" {
		return
	}
	telemetry.Debug(ctx, "qwen inference stream completed",
		inferenceAttrs(op, "", nil, requestID, "")...)
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
