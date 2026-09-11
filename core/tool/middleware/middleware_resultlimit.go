package middleware

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// DefaultResultMarker is appended to a result whose content exceeds the
// configured limit. It is deliberately plain and model-readable:
// truncation is an outcome the model should see and self-correct from,
// not a hidden loss of data.
const DefaultResultMarker = "\n…[result truncated]"

// DefaultResultPartBudget is the total encoded size the non-text parts
// of one tool result may carry before the parts that do not fit are
// dropped. Text has its own rune budget; media and structured data have
// no natural rune count, so they are bounded in bytes instead.
const DefaultResultPartBudget = 1 << 20 // 1 MiB

// ResultLimiter caps how large a tool result may be. Text beyond the
// limit is dropped and DefaultResultMarker (or a custom marker) is
// appended, so the model knows the result was shortened. IsError is
// preserved: truncation is not an error, it is a policy outcome.
//
// Text is metered in runes, spent in order; text past the budget is
// dropped and the marker lands where the cut happened. Non-text parts
// (images, audio, video, files, structured data) carry no rune count,
// so they are metered by encoded size against a separate byte budget
// ([DefaultResultPartBudget], overridable with [WithResultPartBudget]):
// a part that does not fit is dropped and the marker is appended, so
// runaway media cannot fill the model's context unnoticed.
//
// The rune limit is measured in Unicode code points, not bytes, so a
// multibyte result is never cut in the middle of a rune. The marker
// itself counts against that limit; when it is too small to hold the
// full marker, the marker is trimmed to fit.
//
// Place it inside (after) Recover and Telemetry and outside (before)
// Audit so every downstream consumer — including audit — sees the
// final, limited content.
func ResultLimiter(max int, opts ...ResultLimitOption) tool.Middleware {
	if max <= 0 {
		panic(fmt.Sprintf("middleware.ResultLimiter: max must be positive, got %d", max))
	}
	cfg := resultLimitConfig{marker: DefaultResultMarker, partBudget: DefaultResultPartBudget}
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
			return limitResult(next(ctx, call), max, cfg.marker, cfg.partBudget)
		}
	}
}

// ResultLimitOption configures a ResultLimiter.
type ResultLimitOption func(*resultLimitConfig)

type resultLimitConfig struct {
	marker     string
	partBudget int
}

// WithResultMarker replaces the truncation marker appended to limited
// results. An empty marker falls back to DefaultResultMarker.
func WithResultMarker(marker string) ResultLimitOption {
	return func(c *resultLimitConfig) { c.marker = marker }
}

// WithResultPartBudget replaces the byte budget for the non-text parts
// of one result. A non-positive budget lifts the cap, for hosts that
// bound tool results elsewhere.
func WithResultPartBudget(budget int) ResultLimitOption {
	return func(c *resultLimitConfig) { c.partBudget = budget }
}

func limitResult(res message.ToolResult, max int, marker string, partBudget int) message.ToolResult {
	// runeCountAtMost stops at the first rune past the limit, so an
	// oversized result is never converted to a full []rune just to be
	// measured.
	content := res.Content
	if !textRuneCountAtMost(content, max) {
		markerRunes := []rune(marker)
		if len(markerRunes) > max {
			markerRunes = markerRunes[:max]
		}
		keep := max - len(markerRunes)
		if keep < 0 {
			keep = 0
		}
		content = truncateContent(content, keep, string(markerRunes))
	}
	if limited, cut := limitPartBytes(content, partBudget, marker); cut {
		content = limited
	}
	res.Content = content
	return res
}

// textRuneCountAtMost reports whether the text parts of content hold at
// most limit runes. Non-text parts are not counted.
func textRuneCountAtMost(content message.Content, limit int) bool {
	remaining := limit
	for _, part := range content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		text, ok := normalized.(message.TextPart)
		if !ok {
			continue
		}
		if !runeCountAtMost(text.Text, remaining) {
			return false
		}
		remaining -= utf8.RuneCountInString(text.Text)
	}
	return true
}

// truncateContent spends keep runes on the text parts in order,
// dropping text past the budget and preserving every non-text part. It
// returns the content unchanged when no text had to be cut. The marker
// lands on the part where the cut happened, so a result whose budget
// was already spent keeps its part order instead of collecting the
// marker at the end.
func truncateContent(content message.Content, keep int, marker string) message.Content {
	parts := make([]message.Part, 0, len(content.Parts))
	remaining := keep
	truncated := false
	for _, part := range content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		text, ok := normalized.(message.TextPart)
		if !ok {
			parts = append(parts, normalized)
			continue
		}
		if truncated {
			continue
		}
		if length := utf8.RuneCountInString(text.Text); length <= remaining {
			remaining -= length
			parts = append(parts, text)
			continue
		}
		parts = append(parts, message.TextPart{
			Text: truncateRunes(text.Text, remaining) + marker,
		})
		truncated = true
	}
	if !truncated {
		return content
	}
	return message.Content{Parts: parts}
}

// limitPartBytes spends budget bytes on the non-text parts in order,
// dropping the parts that do not fit and appending marker once so the
// loss stays visible. Text parts are not metered here: they carry their
// own rune budget. Content is returned unchanged when nothing was cut.
func limitPartBytes(content message.Content, budget int, marker string) (message.Content, bool) {
	if budget <= 0 {
		return content, false
	}
	parts := make([]message.Part, 0, len(content.Parts))
	remaining := budget
	cut := false
	for _, part := range content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		size, metered := nonTextBytes(normalized)
		if !metered {
			parts = append(parts, normalized)
			continue
		}
		if size > remaining {
			cut = true
			continue
		}
		remaining -= size
		parts = append(parts, normalized)
	}
	if !cut {
		return content, false
	}
	return message.Content{Parts: append(parts, message.TextPart{Text: marker})}, true
}

// nonTextBytes reports how many encoded bytes a part spends against the
// non-text budget, and whether the part is metered at all. Text carries
// its own rune budget; a part that cannot be encoded carries nothing
// the model could receive and is left alone.
func nonTextBytes(part message.Part) (int, bool) {
	if part.Kind() == message.PartText {
		return 0, false
	}
	raw, err := message.MarshalPart(part)
	if err != nil {
		return 0, false
	}
	return len(raw), true
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
