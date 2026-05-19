package queryintent

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Input is the read-path query interpretation contract. Partitioning
// (Scope) is applied on the planner / materialize path, not here.
type Input struct {
	Text      string
	Entities  []string
	Subject   string
	Predicate string
	Object    string
	Kinds     []model.FactKind
	TimeRange model.TimeRange
}

// Compiled is the structured output fed into planner.Input. Explicit
// caller hints win; rule extraction only fills gaps.
type Compiled struct {
	Text      string
	Entities  []string
	Subject   string
	Predicate string
	Object    string
	Kinds     []model.FactKind
	TimeRange model.TimeRange
}

// Compiler enriches a recall.Query before planning.
type Compiler interface {
	Compile(ctx context.Context, input Input) (Compiled, error)
}

// RuleBased is the default deterministic query intent compiler.
type RuleBased struct{}

// Default returns the rule-based compiler wired by recall.New.
func Default() Compiler { return RuleBased{} }

// Compile merges explicit entities with rule-based extraction from Text.
func (RuleBased) Compile(_ context.Context, input Input) (Compiled, error) {
	out := Compiled{
		Text:      input.Text,
		Subject:   input.Subject,
		Predicate: input.Predicate,
		Object:    input.Object,
		Kinds:     append([]model.FactKind(nil), input.Kinds...),
		TimeRange: input.TimeRange,
	}
	out.Entities = mergeEntities(input.Entities, extractEntitiesFromText(input.Text))
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
// so "Who did Alice meet in Paris?" yields alice and paris, not who.
func extractEntitiesFromText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	set := map[string]struct{}{}
	add := func(s string) {
		s = normalizeEntityMention(s)
		if s == "" || isStopword(s) {
			return
		}
		set[s] = struct{}{}
	}
	for _, q := range extractQuotedSpans(text) {
		add(q)
	}
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || (unicode.IsPunct(r) && r != '\'' && r != '-')
	})
	for i, w := range fields {
		runes := []rune(w)
		if len(runes) < 2 {
			continue
		}
		lower := strings.ToLower(w)
		if i == 0 && isStopword(lower) {
			continue
		}
		if unicode.IsUpper(runes[0]) && !isStopword(lower) {
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

func extractQuotedSpans(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	for _, r := range s {
		switch r {
		case '"', '\u201c', '\u201d', '\u300c', '\u300d':
			if in {
				if cur.Len() > 0 {
					out = append(out, cur.String())
				}
				cur.Reset()
				in = false
			} else {
				in = true
			}
		default:
			if in {
				cur.WriteRune(r)
			}
		}
	}
	return out
}

func hasCJKRunes(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) ||
			(r >= 0x3040 && r <= 0x30FF) || // Hiragana/Katakana
			(r >= 0xAC00 && r <= 0xD7AF) { // Hangul
			return true
		}
	}
	return false
}

func isStopword(s string) bool {
	switch s {
	case "who", "whom", "whose", "what", "when", "where", "why", "how",
		"which", "did", "does", "do", "done", "is", "are", "was", "were",
		"am", "be", "been", "being", "have", "has", "had", "having",
		"the", "a", "an", "and", "or", "but", "if", "then", "than",
		"in", "on", "at", "to", "for", "of", "with", "by", "from", "as",
		"into", "about", "after", "before", "between", "during", "over",
		"under", "again", "also", "just", "only", "very", "too", "so",
		"not", "no", "yes", "can", "could", "would", "should", "will",
		"shall", "may", "might", "must", "me", "my", "mine", "you", "your",
		"yours", "he", "him", "his", "she", "her", "hers", "it", "its",
		"we", "us", "our", "ours", "they", "them", "their", "theirs",
		"this", "that", "these", "those", "there", "here",
		"meet", "met", "meeting", "tell", "told", "say", "said", "know", "knew":
		return true
	}
	return false
}
