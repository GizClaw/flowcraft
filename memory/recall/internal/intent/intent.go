package intent

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// RuleBased is the default deterministic query intent compiler.
type RuleBased struct{}

var _ port.IntentCompiler = RuleBased{}

// Default returns the rule-based compiler wired by recall.New.
func Default() port.IntentCompiler { return RuleBased{} }

// Compile merges explicit entities with rule-based extraction from Text.
func (RuleBased) Compile(_ context.Context, input port.IntentInput) (port.IntentResult, error) {
	features := ExtractFeatures(input.Text)
	entities := mergeEntities(input.Entities, extractEntitiesFromText(input.Text))
	out := port.IntentResult{
		Text:      input.Text,
		Subject:   input.Subject,
		Predicate: input.Predicate,
		Object:    input.Object,
		Kinds:     append([]domain.FactKind(nil), input.Kinds...),
		TimeRange: input.TimeRange,
		Entities:  entities,
		Features:  features,
	}
	if out.TimeRange.IsZero() {
		out.TimeRange = features.Temporal.TimeRange
	}
	if len(out.Kinds) == 0 {
		out.Kinds = inferKinds(features)
	}
	if out.Subject == "" && shouldInferSubject(input.Text, features) {
		out.Subject = inferSubject(input.Text, entities)
	}
	return out, nil
}

func mergeEntities(explicit, extracted []string) []string {
	seen := make(map[string]struct{}, len(explicit)+len(extracted))
	add := func(s string) []string {
		s = words.NormalizeIntentEntityMention(s)
		if s == "" {
			return nil
		}
		if _, ok := seen[s]; ok {
			return nil
		}
		seen[s] = struct{}{}
		return []string{s}
	}
	var out []string
	for _, e := range explicit {
		out = append(out, add(e)...)
	}
	for _, e := range extracted {
		out = append(out, add(e)...)
	}
	return out
}

// extractEntitiesFromText is a conservative rule baseline: quoted spans,
// capitalized tokens, and CJK runs. Common question words are filtered
// (via recall/internal/words) so "Who did Alice meet in Paris?" yields alice
// and paris, not who.
func extractEntitiesFromText(text string) []string {
	return words.ExtractIntentEntityMentions(text)
}

func inferKinds(features domain.QueryFeatures) []domain.FactKind {
	if features.Temporal.HasIntent {
		return []domain.FactKind{domain.KindEvent, domain.KindState, domain.KindPlan}
	}
	return nil
}

func inferSubject(text string, entities []string) string {
	if len(entities) == 0 {
		return ""
	}
	lower := strings.ToLower(text)
	best := ""
	bestIdx := len(lower) + 1
	for _, e := range entities {
		e = words.NormalizeIntentEntityMention(e)
		if e == "" {
			continue
		}
		idx := indexEntityMention(lower, e)
		if idx >= 0 && idx < bestIdx {
			best = e
			bestIdx = idx
		}
	}
	if best != "" {
		return best
	}
	return entities[0]
}

func indexEntityMention(lowerText, entity string) int {
	start := 0
	for {
		idx := strings.Index(lowerText[start:], entity)
		if idx < 0 {
			return -1
		}
		idx += start
		if hasEntityBoundary(lowerText, idx, len(entity)) {
			return idx
		}
		start = idx + len(entity)
		if start >= len(lowerText) {
			return -1
		}
	}
}

func hasEntityBoundary(text string, start, length int) bool {
	beforeOK := start == 0
	if !beforeOK {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		beforeOK = !isEntityBoundaryRune(r)
	}
	end := start + length
	afterOK := end >= len(text)
	if !afterOK {
		r, _ := utf8.DecodeRuneInString(text[end:])
		afterOK = !isEntityBoundaryRune(r)
	}
	return beforeOK && afterOK
}

func isEntityBoundaryRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func shouldInferSubject(text string, features domain.QueryFeatures) bool {
	if features.Temporal.HasIntent {
		return false
	}
	return words.HasSubjectInferenceCue(text)
}

func hasCJKRunes(s string) bool {
	for _, r := range s {
		if tokenize.IsCJK(r) {
			return true
		}
	}
	return false
}
