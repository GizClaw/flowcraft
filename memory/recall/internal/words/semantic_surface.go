package words

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/text/normalize"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

var semanticTokenizer = tokenize.NewMultilingual()

// CanonicalSurface is the common lowercase, punctuation-insensitive text form
// for lightweight query/source cue matching.
func CanonicalSurface(text string) string {
	text = normalize.ReplaceNonAlnumWithSpace(text)
	text = normalize.CollapseSpaces(text)
	return strings.ToLower(text)
}

// SemanticQueryTerms returns deduplicated query tokens in the same multilingual
// vocabulary used by memory/text.
func SemanticQueryTerms(text string) []string {
	tokens := semanticTokenizer.Tokenize(text)
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if len([]rune(token)) < 2 {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}
