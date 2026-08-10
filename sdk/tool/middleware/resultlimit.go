package middleware

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// DefaultResultMarker is appended to a result whose content exceeds the
// configured limit. It is deliberately plain and model-readable:
// truncation is an outcome the model should see and self-correct from,
// not a hidden loss of data.
const DefaultResultMarker = "\n…[result truncated]"

// ResultLimiter caps how long a tool result may be. Content beyond the
// limit is dropped and DefaultResultMarker (or a custom marker) is
// appended, so the model knows the result was shortened. IsError is
// preserved: truncation is not an error, it is a policy outcome.
//
// The limit is measured in Unicode code points, not bytes, so a
// multibyte result is never cut in the middle of a rune. The marker
// itself counts against the limit; when the limit is too small to hold
// the full marker, the marker is trimmed to fit.
//
// Place it inside (after) Recover and Telemetry and outside (before)
// Audit so every downstream consumer — including audit — sees the
// final, limited content.
func ResultLimiter(max int, opts ...ResultLimitOption) tool.Middleware {
	if max <= 0 {
		panic(fmt.Sprintf("middleware.ResultLimiter: max must be positive, got %d", max))
	}
	cfg := resultLimitConfig{marker: DefaultResultMarker}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.marker == "" {
		cfg.marker = DefaultResultMarker
	}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.Call) message.Result {
			return limitResult(next(ctx, call), max, cfg.marker)
		}
	}
}

// ResultLimitOption configures a ResultLimiter.
type ResultLimitOption func(*resultLimitConfig)

type resultLimitConfig struct {
	marker string
}

// WithResultMarker replaces the truncation marker appended to limited
// results. An empty marker falls back to DefaultResultMarker.
func WithResultMarker(marker string) ResultLimitOption {
	return func(c *resultLimitConfig) { c.marker = marker }
}

func limitResult(res message.Result, max int, marker string) message.Result {
	runes := []rune(res.Content)
	if len(runes) <= max {
		return res
	}
	markerRunes := []rune(marker)
	if len(markerRunes) > max {
		markerRunes = markerRunes[:max]
	}
	keep := max - len(markerRunes)
	if keep < 0 {
		keep = 0
	}
	out := make([]rune, 0, max)
	out = append(out, runes[:keep]...)
	out = append(out, markerRunes...)
	res.Content = string(out)
	return res
}
