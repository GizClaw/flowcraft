package middleware

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
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
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
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

func limitResult(res message.ToolResult, max int, marker string) message.ToolResult {
	// Fast paths avoid materializing the whole content as []rune: a
	// byte count at or below the limit can never exceed it in runes,
	// and runeCountAtMost stops at the first rune past the limit.
	if len(res.Content) <= max || runeCountAtMost(res.Content, max) {
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
	res.Content = truncateRunes(res.Content, keep) + string(markerRunes)
	return res
}

// runeCountAtMost reports whether s contains at most limit runes. It
// stops counting as soon as the answer is known, so oversized results
// are never converted to a full []rune just to be truncated.
func runeCountAtMost(s string, limit int) bool {
	if limit < 0 {
		return false
	}
	count := 0
	for range s {
		count++
		if count > limit {
			return false
		}
	}
	return true
}

// truncateRunes returns the first n runes of s without converting the
// whole string to []rune.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n) // capacity hint only; Builder grows as writes come in
	remaining := n
	for _, r := range s {
		if remaining == 0 {
			break
		}
		b.WriteRune(r)
		remaining--
	}
	return b.String()
}
