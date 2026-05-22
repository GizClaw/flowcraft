package tokenize

import (
	"strings"
	"unicode"
)

// SplitProperNouns splits text on Unicode letter / digit boundaries,
// like [SplitWords], BUT keeps ASCII apostrophe (') and hyphen (-)
// as internal characters of a token. Edge apostrophes / hyphens are
// trimmed.
//
// This is the right primitive for proper-noun NER over English-like
// scripts where compound names — "O'Brien", "Jean-Luc", "Saint-
// Étienne" — must survive as single tokens. [SplitWords] splits on
// those punctuation characters and would fragment such names into
// useless capitalised letter fragments ("O", "Brien").
//
// The function intentionally does NOT lower-case and does NOT drop
// short tokens; the caller is expected to layer case / length /
// stop-word policy on top. Output preserves the original surface
// case so heuristic NER passes (TitleCased detection, KnownEntities
// substring match) see the unfolded form.
//
// Tokens that would consist only of apostrophes / hyphens after
// trimming are dropped.
func SplitProperNouns(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		tok := strings.Trim(cur.String(), "'-")
		if tok != "" {
			out = append(out, tok)
		}
		cur.Reset()
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '-':
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}
