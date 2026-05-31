package pipeline

import (
	"context"
	"sort"
	"strings"
	"unicode"

	base "github.com/GizClaw/flowcraft/memory/retrieval/pipeline"
)

// EntityExtract extracts proper nouns, quoted strings and CJK compounds from
// the query text. Provide LLMExtractor to override the rule-based extractor.
type EntityExtract struct {
	LLMExtractor func(ctx context.Context, text string) ([]string, error)
}

func (s EntityExtract) Name() string { return "EntityExtract" }

func (s EntityExtract) Run(ctx context.Context, st *base.State) error {
	if st.Request == nil {
		return nil
	}
	if s.LLMExtractor != nil {
		out, err := s.LLMExtractor(ctx, st.Request.QueryText)
		if err != nil {
			return err
		}
		st.QueryEntities = dedupStringsLower(out)
		return nil
	}
	st.QueryEntities = ruleEntities(st.Request.QueryText)
	return nil
}

func ruleEntities(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	set := map[string]struct{}{}
	add := func(s string) {
		s = strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) })
		if len(s) < 2 {
			return
		}
		set[strings.ToLower(s)] = struct{}{}
	}
	for _, q := range extractQuoted(text) {
		add(q)
	}
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || (unicode.IsPunct(r) && r != '\'' && r != '-')
	}) {
		runes := []rune(w)
		if len(runes) >= 2 && unicode.IsUpper(runes[0]) {
			add(w)
		}
		if hasCJK(w) && len([]rune(w)) >= 2 {
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

func extractQuoted(s string) []string {
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

func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

func dedupStringsLower(in []string) []string {
	set := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(strings.ToLower(s))
		if s == "" {
			continue
		}
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
