package pack

import (
	"unicode"

	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// EstimatedTokens estimates the token cost of text without a tokenizer. The
// unit is a quarter token: ASCII runes cost one unit, other non-ASCII runes
// two units (half a token), and CJK/Hangul runes four units (one token). The
// estimate is deliberately conservative for non-Latin scripts, where a plain
// "runes / 4" heuristic undercounts by up to 4x. Pure ASCII keeps the
// historical ceil(runes/4) result.
//
// This is the module's one cost model, and it is not the tokenizer the
// retrieval lanes match with: matching is the shared kernel's
// (core/utils/bm25), which splits CJK into single characters plus adjacent
// bigrams so a fragment stays matchable, counting one CJK character as up to
// three terms. Cost is what a provider bills, where one CJK character is
// roughly one token, so budgets — this package's packing, the retrieval
// budgets, the recent lane — are all bounded by this estimator and never by
// the kernel's tokenizer.
func EstimatedTokens(text string) int {
	if text == "" {
		return 0
	}
	units := 0
	for _, r := range text {
		units += runeUnits(r)
	}
	return max(1, (units+3)/4)
}

// ContentTokens estimates the token cost of message content: the estimated
// cost of its text parts, or one token for content that carries no text but
// does carry parts. Every budget site accounts for content this way, so one
// way of counting keeps a payload's cost the same wherever it is bounded.
func ContentTokens(content coremessage.Content) int {
	text := content.Text()
	if text == "" {
		if len(content.Parts) == 0 {
			return 0
		}
		return 1
	}
	return EstimatedTokens(text)
}

// TruncateToTokens returns the longest rune prefix of text whose estimated
// token cost does not exceed maxTokens, and reports whether it cut anything.
// A non-positive maxTokens yields an empty string.
func TruncateToTokens(text string, maxTokens int) (string, bool) {
	if maxTokens <= 0 {
		return "", text != ""
	}
	limit := maxTokens * 4
	units := 0
	for index, r := range text {
		weight := runeUnits(r)
		if units+weight > limit {
			return text[:index], true
		}
		units += weight
	}
	return text, false
}

// runeUnits returns the quarter-token weight of one rune.
func runeUnits(r rune) int {
	switch {
	case r < 0x80:
		return 1
	case isWideRune(r):
		return 4
	default:
		return 2
	}
}

// isWideRune reports whether r is a CJK/Hangul/fullwidth rune that typically
// costs a full token in modern tokenizers.
func isWideRune(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK punctuation
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // fullwidth forms
		return true
	case r >= 0x20000 && r <= 0x3FFFF: // CJK extension B and beyond
		return true
	default:
		return false
	}
}
