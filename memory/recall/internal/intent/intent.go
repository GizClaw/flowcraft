package intent

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
	"github.com/GizClaw/flowcraft/memory/text/quotes"
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
		s = normalizeEntityMention(s)
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

func normalizeEntityMention(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})
	if len(s) < 2 {
		return ""
	}
	return strings.ToLower(s)
}

// extractEntitiesFromText is a conservative rule baseline: quoted spans,
// capitalized tokens, and CJK runs. Common question words are filtered
// (via recall/internal/words) so "Who did Alice meet in Paris?" yields alice
// and paris, not who.
func extractEntitiesFromText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	set := map[string]struct{}{}
	add := func(s string) {
		s = normalizeEntityMention(s)
		if s == "" || words.IsIntentEntityStopword(s) {
			return
		}
		set[s] = struct{}{}
	}
	for _, q := range quotes.ExtractSpans(text) {
		add(q)
	}
	// FieldsFunc keeps apostrophe / hyphen inside tokens so names
	// like "O'Brien" and "Jean-Luc" survive as single mentions —
	// tokenize.SplitWords splits on those, so we cannot use it
	// directly here.
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || (unicode.IsPunct(r) && r != '\'' && r != '-')
	})
	for i, w := range fields {
		runes := []rune(w)
		if len(runes) < 2 {
			continue
		}
		lower := strings.ToLower(w)
		if i == 0 && words.IsIntentEntityStopword(lower) {
			continue
		}
		if unicode.IsUpper(runes[0]) && !words.IsIntentEntityStopword(lower) {
			add(w)
		}
		if hasCJKRunes(w) && len(runes) >= 2 {
			add(w)
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
		e = normalizeEntityMention(e)
		if e == "" {
			continue
		}
		idx := strings.Index(lower, e)
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
