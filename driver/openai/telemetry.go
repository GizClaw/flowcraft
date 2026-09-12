package openai

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// providerID is this driver's provider identity: it namespaces every extension
// the package defines, labels the compile errors the ledger builds, and tags
// telemetry. The runtime qualifies extension fields with ProviderID and
// rejects extensions whose provider does not match the resolved model's
// deployment provider, so a deployment that names its provider differently
// must set the Provider field on the options structs it attaches.
const providerID = "openai"

// logChatFinishSynthesized warns when a chat stream ended cleanly without an
// explicit finish_reason and the adapter fabricated the terminal reason. The
// provider (or an intermediary) may have truncated the stream mid-flight, so
// the synthesized finish must not be silently trusted. Every other provider
// event comes from inference.LogProvider*; this one is chat-specific.
func logChatFinishSynthesized(
	ctx context.Context,
	model, requestID, responseID, synthesized string,
) {
	attrs := append(
		inference.ProviderLogAttrs(
			providerID, "generate", model, nil, requestID, responseID,
		),
		otellog.String("stream.finish.synthesized", synthesized),
	)
	telemetry.Warn(ctx,
		"chat stream ended without a finish reason; finish synthesized",
		attrs...)
}
