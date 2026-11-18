package middleware

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	"go.opentelemetry.io/otel/attribute"
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

// DefaultResultStructuredBudget bounds the structured-data parts of one result:
// the JSON a tool hands back, as opposed to media, whose size is what it is.
//
// Structured data had neither a natural rune count (it is a document, not prose)
// nor a budget that scaled with the text budget: the media budget simply applied,
// at a megabyte, and a part that did not fit was dropped whole. This is the
// smaller default the JSON a model reads deserves.
const DefaultResultStructuredBudget = 64 << 10 // 64 KiB

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
// For a recoverable cap — the full output spilled to disk and an excerpt
// plus pointer left in context — see [Truncate], which is designed to run
// inside this limiter.
//
// The two budgets share one promise: a result that was cut never carries
// more than max runes of text. The marker counts against that budget, so a
// result whose non-text parts were cut keeps the marker's runes free instead
// of adding them on top.
//
// The rune limit is measured in Unicode code points, not bytes, so a
// multibyte result is never cut in the middle of a rune. The marker
// itself counts against that limit; when it is too small to hold the
// full marker, the marker is trimmed to fit.
//
// Place it inside (after) Recover and Telemetry and outside (before)
// Audit so every downstream consumer — including audit — sees the
// final, limited content.
//
// A non-positive max is reported as an errdefs.Validation error;
// [MustResultLimiter] is the panicking form.
func ResultLimiter(max int, opts ...ResultLimitOption) (tool.Middleware, error) {
	if max <= 0 {
		return nil, errdefs.Validationf(
			"middleware.ResultLimiter: max must be positive, got %d", max)
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
	if !cfg.structuredSet {
		if cfg.partSet {
			cfg.structuredBudget = cfg.partBudget
		} else {
			cfg.structuredBudget = derivedStructuredBudget(max)
		}
	}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			res := next(ctx, call)
			limited := limitResult(res, max, cfg.marker, cfg.partBudget, cfg.structuredBudget)
			// A shortened result is a context-budget outcome the model has to
			// live with; recording it on the span is what makes a bad answer
			// traceable to this middleware rather than to the model.
			before := utf8.RuneCountInString(res.Content.Text())
			after := utf8.RuneCountInString(limited.Content.Text())
			if after < before {
				annotate(ctx,
					attribute.Bool(telemetry.AttrResultTruncated, true),
					attribute.Int(telemetry.AttrResultDroppedRunes, before-after))
			}
			return limited
		}
	}, nil
}

// MustResultLimiter is [ResultLimiter] for package-level configuration, where
// a rejected limit is a programmer error: it panics instead of returning the
// validation error.
func MustResultLimiter(max int, opts ...ResultLimitOption) tool.Middleware {
	limiter, err := ResultLimiter(max, opts...)
	if err != nil {
		panic(err)
	}
	return limiter
}

// ResultLimitOption configures a ResultLimiter.
type ResultLimitOption func(*resultLimitConfig)

type resultLimitConfig struct {
	marker     string
	partBudget int
	partSet    bool
	// structuredBudget bounds structured-data parts. Zero lifts the cap, which is
	// why partSet/structuredSet exist: "unset" and "lifted" are different answers.
	structuredBudget int
	structuredSet    bool
}

// WithResultMarker replaces the truncation marker appended to limited
// results. An empty marker falls back to DefaultResultMarker.
func WithResultMarker(marker string) ResultLimitOption {
	return func(c *resultLimitConfig) { c.marker = marker }
}

// WithResultPartBudget replaces the byte budget for the non-text parts
// of one result. A non-positive budget lifts the cap, for hosts that
// bound tool results elsewhere.
//
// It governs media and, unless [WithResultStructuredBudget] says otherwise,
// structured data too, which is what it always did.
func WithResultPartBudget(budget int) ResultLimitOption {
	return func(c *resultLimitConfig) {
		c.partBudget, c.partSet = budget, true
	}
}

// WithResultStructuredBudget replaces the byte budget for the structured-data
// parts (JSON) of one result, independently of the media budget. A non-positive
// budget lifts that cap.
//
// Unset, structured data uses the media budget when the host set one, and
// otherwise a budget derived from the rune limit ([ResultLimiter]) — a host that
// allows 20 000 runes of text is not asking for a megabyte of JSON.
func WithResultStructuredBudget(budget int) ResultLimitOption {
	return func(c *resultLimitConfig) {
		c.structuredBudget, c.structuredSet = budget, true
	}
}

// ResultPartLimiter bounds the non-text parts of one result without touching
// its text. It exists for hosts that leave text sizes to their own policy but
// still need media to be bounded: a tool result rides every later turn's
// context, and an inline image or audio payload has no natural size. Parts
// that do not fit the byte budget are dropped and [DefaultResultMarker] is
// appended, so the loss stays visible to the model.
//
// A non-positive budget is reported as an errdefs.Validation error;
// [MustResultPartLimiter] is the panicking form.
func ResultPartLimiter(budget int, opts ...ResultLimitOption) (tool.Middleware, error) {
	if budget <= 0 {
		return nil, errdefs.Validationf(
			"middleware.ResultPartLimiter: budget must be positive, got %d", budget)
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
	if !cfg.structuredSet {
		// No rune limit to scale from: one budget for every non-text part, which
		// is what this limiter always meant.
		cfg.structuredBudget = budget
	}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			res := next(ctx, call)
			limited, cut := limitPartBytes(res.Content, budget, cfg.structuredBudget)
			// The rebuilt content reaches the caller even when no marker is due: a
			// structured part replaced by a note explains itself, and dropping the
			// rebuild here would silently keep the original.
			res.Content = limited
			if !cut {
				return res
			}
			res.Content = message.Content{Parts: append(
				append([]message.Part(nil), limited.Parts...),
				message.TextPart{Text: cfg.marker},
			)}
			return res
		}
	}, nil
}

// MustResultPartLimiter is [ResultPartLimiter] for package-level
// configuration, where a rejected budget is a programmer error: it panics
// instead of returning the validation error.
func MustResultPartLimiter(budget int, opts ...ResultLimitOption) tool.Middleware {
	limiter, err := ResultPartLimiter(budget, opts...)
	if err != nil {
		panic(err)
	}
	return limiter
}

func limitResult(res message.ToolResult, max int, marker string, partBudget, structuredBudget int) message.ToolResult {
	// runeCountAtMost stops at the first rune past the limit, so an
	// oversized result is never converted to a full []rune just to be
	// measured. Non-text parts are metered first because they never carry
	// the marker: cutting them reserves the marker's runes up front, which
	// is what keeps the text budget true when media and text are both over.
	content := res.Content
	content, partsCut := limitPartBytes(content, partBudget, structuredBudget)
	markerRunes := []rune(marker)
	if len(markerRunes) > max {
		markerRunes = markerRunes[:max]
	}
	keep := max - len(markerRunes)
	if keep < 0 {
		keep = 0
	}
	switch {
	case partsCut:
		// The marker goes at the end, where the parts were dropped, so the
		// text parts are cut to leave room for it and are cut silently: the
		// end marker is the one signal the model needs, and two markers would
		// not fit the budget.
		content = truncateContent(content, keep, "")
		content.Parts = append(
			append([]message.Part(nil), content.Parts...),
			message.TextPart{Text: string(markerRunes)},
		)
	case !textRuneCountAtMost(content, max):
		content = truncateContent(content, keep, string(markerRunes))
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
		// A cut that leaves neither text nor a marker contributes nothing —
		// the caller's own marker is the signal — so the part is dropped
		// rather than carried as an empty text part.
		if kept := truncateRunes(text.Text, remaining) + marker; kept != "" {
			parts = append(parts, message.TextPart{Text: kept})
		}
		truncated = true
	}
	if !truncated {
		return content
	}
	return message.Content{Parts: parts}
}

// limitPartBytes spends budget bytes on the non-text parts in order,
// dropping the parts that do not fit. Text parts are not metered here: they
// carry their own rune budget. Content is returned unchanged when nothing was
// cut; the caller appends the truncation marker, because only it knows how
// much text budget the marker still has to fit into.
func limitPartBytes(content message.Content, budget, structuredBudget int) (message.Content, bool) {
	if budget <= 0 && structuredBudget <= 0 {
		return content, false
	}
	mediaLifted := budget <= 0
	structuredLifted := structuredBudget <= 0
	parts := make([]message.Part, 0, len(content.Parts))
	remaining := budget
	// cut is "a part was dropped and the model needs the marker"; changed is
	// "the rebuilt list differs at all". They are not the same: a structured part
	// replaced by a note explains itself, so it must not double up with a marker —
	// but the rebuilt list still has to reach the caller.
	cut := false
	changed := false
	for _, part := range content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			changed = true
			continue
		}
		size, metered := nonTextBytes(normalized)
		if !metered {
			parts = append(parts, normalized)
			continue
		}
		if normalized.Kind() == message.PartData {
			// Structured data has its own budget — lifted or not — and never
			// spends the media one.
			if structuredLifted {
				parts = append(parts, normalized)
				continue
			}
			// A document cannot be shortened without breaking its shape, but it
			// can be accounted for: the note names the loss, where dropping the
			// part said only "something was cut".
			if size > structuredBudget {
				parts = append(parts, message.TextPart{
					Text: omittedStructuredNote(size, structuredBudget),
				})
				changed = true
				continue
			}
			structuredBudget -= size
			parts = append(parts, normalized)
			continue
		}
		if !mediaLifted && size > remaining {
			cut, changed = true, true
			continue
		}
		remaining -= size
		parts = append(parts, normalized)
	}
	if !changed {
		return content, false
	}
	return message.Content{Parts: parts}, cut
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

// derivedStructuredBudget scales the structured-data budget to the rune budget: a
// host that allows max runes of text is not asking for a megabyte of JSON. It never
// exceeds [DefaultResultStructuredBudget], and it stays usable rather than
// degenerate for a very small text budget.
func derivedStructuredBudget(max int) int {
	// Clamp before multiplying: 4*max wraps negative for a rune budget near
	// MaxInt, and the floor below would then return the smallest budget for the
	// largest one.
	if max > DefaultResultStructuredBudget/4 {
		return DefaultResultStructuredBudget
	}
	derived := 4 * max
	if derived < 1<<10 {
		derived = 1 << 10
	}
	return derived
}

// omittedStructuredNote names a structured part the budget could not keep, so the
// model knows a result existed and how big it was.
//
// It is kept to one short line on purpose: the note is text, so it spends the rune
// budget, and a text budget small enough to cut it is one that could not carry a
// tool result at all. The sizes stay in it because they are what tells the model
// whether to retry smaller.
func omittedStructuredNote(size, budget int) string {
	return "\n…[structured result omitted: " + humanBytes(size) +
		" > " + humanBytes(budget) + "]"
}

// humanBytes renders a byte count for a message a model reads.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
