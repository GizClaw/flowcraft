package llm

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// WithLimits wraps inner so that numeric GenerateOptions fields
// exceeding the spec's hard ceilings are clamped down (with a
// telemetry warning), not erroring. See ModelLimits doc and the
// design RFC §10.3 for the clamp-vs-error decision.
//
// If limits is the zero value (no fields set), inner is returned
// unwrapped — the resolver relies on this to skip the wrap entirely
// when ModelSpec.Limits is empty.
func WithLimits(inner LLM, limits ModelLimits) LLM {
	if limits.IsZero() {
		return inner
	}
	return &limitsLLM{inner: inner, limits: limits}
}

type limitsLLM struct {
	inner  LLM
	limits ModelLimits
}

func (l *limitsLLM) Generate(ctx context.Context, msgs []Message, opts ...GenerateOption) (Message, TokenUsage, error) {
	return l.inner.Generate(ctx, msgs, l.clamp(ctx, opts)...)
}

func (l *limitsLLM) GenerateStream(ctx context.Context, msgs []Message, opts ...GenerateOption) (StreamMessage, error) {
	return l.inner.GenerateStream(ctx, msgs, l.clamp(ctx, opts)...)
}

// clamp appends a final option that inspects the assembled
// GenerateOptions and tightens any field exceeding the limit. The
// option runs LAST so it sees the post-defaults / post-caller
// merged value.
func (l *limitsLLM) clamp(ctx context.Context, opts []GenerateOption) []GenerateOption {
	out := make([]GenerateOption, 0, len(opts)+1)
	out = append(out, opts...)
	out = append(out, func(o *GenerateOptions) {
		if l.limits.MaxOutputTokens > 0 && o.MaxTokens != nil && *o.MaxTokens > l.limits.MaxOutputTokens {
			telemetry.Warn(ctx, "llm: clamping MaxTokens to model limit",
				otellog.Int64("requested", *o.MaxTokens),
				otellog.Int64("limit", l.limits.MaxOutputTokens))
			cap := l.limits.MaxOutputTokens
			o.MaxTokens = &cap
		}
		if l.limits.MaxToolDefinitions > 0 && len(o.Tools) > l.limits.MaxToolDefinitions {
			telemetry.Warn(ctx, "llm: truncating Tools to model limit",
				otellog.Int("requested", len(o.Tools)),
				otellog.Int("limit", l.limits.MaxToolDefinitions))
			o.Tools = o.Tools[:l.limits.MaxToolDefinitions]
		}
		// MaxContextTokens is informational only — there is no built-in
		// tokenizer to enforce it. Pod / app-level probes can read the
		// value off the catalog if they want preemptive rejection.
	})
	return out
}
