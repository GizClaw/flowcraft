package intent

import (
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
	"github.com/GizClaw/flowcraft/memory/text/quotes"
	"github.com/GizClaw/flowcraft/memory/text/timex"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// ExtractFeatures returns the shared query-understanding features for a recall
// query. It is part of the intent pipeline; non-intent callers should prefer
// consuming QueryIntent.Features instead of calling this directly.
func ExtractFeatures(text string) domain.QueryFeatures {
	return ExtractFeaturesAt(text, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
}

// ExtractFeaturesAt is ExtractFeatures with an explicit anchor for
// relative-time parsers.
func ExtractFeaturesAt(text string, anchor time.Time) domain.QueryFeatures {
	if anchor.IsZero() {
		anchor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	features := domain.QueryFeatures{
		Tokens:            TextTokenSet(text),
		Numeric:           NumericTokens(text),
		Quoted:            QuotedTokenSet(text),
		Proper:            ProperNounSet(text),
		NumericIntentKind: numericIntentKinds(text),
	}
	features.NumericIntent = len(features.NumericIntentKind) > 0
	features.Temporal = extractTemporal(text, anchor)
	if features.Temporal.HasDurationIntent {
		features.NumericIntent = true
		if !slices.Contains(features.NumericIntentKind, domain.QueryNumericIntentDuration) {
			features.NumericIntentKind = append(features.NumericIntentKind, domain.QueryNumericIntentDuration)
		}
	}
	return features
}

func extractTemporal(text string, anchor time.Time) domain.QueryTemporalFeatures {
	out := domain.QueryTemporalFeatures{
		HasIntent:         hasTemporalIntent(text),
		HasDurationIntent: hasDurationIntent(text),
		IntentKind:        temporalIntentKinds(text),
	}
	expr, err := extractTimex(text, anchor)
	if err != nil || expr == nil {
		return out
	}
	out.MatchedText = expr.Text
	out.HasRelativeExpression = expr.Relative
	if expr.HasRange {
		out.HasExplicitDate = true
		out.TimeRange = rangeFromTimexExpression(expr)
		if !slices.Contains(out.IntentKind, domain.QueryTemporalIntentDate) {
			out.IntentKind = append(out.IntentKind, domain.QueryTemporalIntentDate)
		}
	}
	return out
}

func rangeFromTimexExpression(expr *timex.Expression) domain.TimeRange {
	if expr == nil || !expr.HasRange {
		return domain.TimeRange{}
	}
	return domain.TimeRange{From: expr.Start.UTC(), To: expr.End.UTC()}
}

// HasTimex reports whether text contains a parseable absolute or natural time
// expression. It is intended for candidate/evidence text, not query intent.
func HasTimex(text string, anchor time.Time) bool {
	expr, err := extractTimex(text, anchor)
	return err == nil && expr != nil
}

func extractTimex(text string, anchor time.Time) (*timex.Expression, error) {
	return timex.Extract(text, anchor)
}

func hasTemporalIntent(text string) bool {
	return words.HasTemporalQuestionCue(text)
}

func hasDurationIntent(text string) bool {
	return words.HasDurationQuestionCue(text)
}

func temporalIntentKinds(text string) []domain.QueryTemporalIntentKind {
	return words.TemporalIntentKinds(text)
}

func numericIntentKinds(text string) []domain.QueryNumericIntentKind {
	return words.NumericIntentKinds(text)
}

// TokenSet converts a token slice into a set.
func TokenSet(tokens []string) map[string]struct{} {
	out := make(map[string]struct{}, len(tokens))
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}

// TextTokenSet tokenizes free text with the recall query-understanding
// tokenizer. CJK keeps bigram tokenisation; other scripts use the multilingual
// Simple tokenizer so query and evidence scoring share the same folding.
func TextTokenSet(text string) map[string]struct{} {
	if hasCJKRunes(text) {
		return TokenSet(tokenize.Detect(text).Tokenize(text))
	}
	return TokenSet(tokenize.NewMultilingual().Tokenize(text))
}

// NumericTokens extracts contiguous numeric token surfaces.
func NumericTokens(text string) map[string]struct{} {
	out := map[string]struct{}{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out[cur.String()] = struct{}{}
		cur.Reset()
	}
	for _, r := range text {
		if unicode.IsDigit(r) || unicode.IsNumber(r) {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// QuotedTokenSet tokenizes explicitly quoted spans.
func QuotedTokenSet(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, span := range quotes.ExtractSpans(text) {
		for tok := range TextTokenSet(span) {
			out[tok] = struct{}{}
		}
	}
	return out
}

// ProperNounSet extracts simple title-cased proper-noun tokens.
func ProperNounSet(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, tok := range tokenize.SplitProperNouns(text) {
		if !isTitleCased(tok) {
			continue
		}
		out[strings.ToLower(tok)] = struct{}{}
	}
	return out
}

func isTitleCased(tok string) bool {
	if len(tok) < 2 {
		return false
	}
	runes := []rune(tok)
	if !unicode.IsUpper(runes[0]) {
		return false
	}
	return slices.ContainsFunc(runes[1:], unicode.IsLower)
}
