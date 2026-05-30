// Package phrase provides token-boundary phrase matching for higher-level text
// understanding code that should not hand-roll string scans.
package phrase

import (
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/memory/text/lemma"
	"github.com/GizClaw/flowcraft/memory/text/normalize"
	snowball "github.com/GizClaw/flowcraft/memory/text/stem/adapter/snowball"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// Matcher is a normalised view of text for phrase-level predicates.
type Matcher struct {
	raw    string
	tokens []string
}

// New builds a phrase matcher using Unicode normalisation, raw word boundaries,
// language-scoped lemmatisation, and Snowball stemming where available. It
// intentionally does not drop stop words because many intent cues are stop words
// ("when", "how").
func New(text string) Matcher {
	raw := strings.ToLower(normalize.CollapseSpaces(normalize.NFC(text)))
	words := tokenize.SplitWords(raw)
	tokens := make([]string, 0, len(words))
	for _, word := range words {
		if tok := foldWord(word); tok != "" {
			tokens = append(tokens, tok)
		}
	}
	return Matcher{raw: raw, tokens: tokens}
}

// Tokens returns a copy of the folded tokens used by this matcher.
func (m Matcher) Tokens() []string {
	out := make([]string, len(m.tokens))
	copy(out, m.tokens)
	return out
}

// Contains reports whether token occurs as a folded token, respecting word
// boundaries.
func (m Matcher) Contains(token string) bool {
	want := foldWord(token)
	if want == "" {
		return false
	}
	return slices.Contains(m.tokens, want)
}

// ContainsAny reports whether any token occurs as a folded token.
func (m Matcher) ContainsAny(tokens ...string) bool {
	return slices.ContainsFunc(tokens, m.Contains)
}

// ContainsPhrase reports whether a folded token sequence occurs contiguously.
func (m Matcher) ContainsPhrase(tokens ...string) bool {
	want := foldWords(tokens...)
	if len(want) == 0 || len(want) > len(m.tokens) {
		return false
	}
	for i := 0; i+len(want) <= len(m.tokens); i++ {
		ok := true
		for j := range want {
			if m.tokens[i+j] != want[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// StartsWithPhrase reports whether the folded token stream starts with tokens.
func (m Matcher) StartsWithPhrase(tokens ...string) bool {
	want := foldWords(tokens...)
	if len(want) == 0 || len(want) > len(m.tokens) {
		return false
	}
	for i := range want {
		if m.tokens[i] != want[i] {
			return false
		}
	}
	return true
}

// ContainsLiteral reports whether raw occurs in the normalised lower-cased
// text. Prefer token predicates for whitespace-delimited languages; this exists
// for scripts where cue phrases are not separated by spaces.
func (m Matcher) ContainsLiteral(raw string) bool {
	return m.IndexLiteral(raw) >= 0
}

// ContainsAnyLiteral reports whether any raw literal occurs in the normalised
// lower-cased text.
func (m Matcher) ContainsAnyLiteral(literals ...string) bool {
	for _, literal := range literals {
		if m.ContainsLiteral(literal) {
			return true
		}
	}
	return false
}

// IndexLiteral returns the byte index of raw in the normalised lower-cased text,
// or -1 when absent.
func (m Matcher) IndexLiteral(raw string) int {
	needle := strings.ToLower(normalize.CollapseSpaces(normalize.NFC(raw)))
	if needle == "" {
		return -1
	}
	return strings.Index(m.raw, needle)
}

func foldWords(tokens ...string) []string {
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		for _, word := range tokenize.SplitWords(token) {
			if folded := foldWord(word); folded != "" {
				out = append(out, folded)
			}
		}
	}
	return out
}

func foldWord(word string) string {
	word = strings.ToLower(normalize.CollapseSpaces(normalize.NFC(word)))
	if word == "" {
		return ""
	}
	return stemFirstWithLanguageLemma(word, "english", "spanish", "french", "russian")
}

func stemFirstWithLanguageLemma(word string, langs ...string) string {
	best := ""
	for _, lang := range langs {
		normalised := lemma.LemmatizeLang(word, lang)
		stemmed, err := snowball.StemLang(normalised, lang, false)
		if err != nil || stemmed == "" || stemmed == word {
			continue
		}
		if best == "" || len([]rune(stemmed)) < len([]rune(best)) {
			best = stemmed
		}
	}
	if best != "" {
		return best
	}
	return word
}
