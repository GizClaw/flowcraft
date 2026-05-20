package tokenize

import (
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/text/lemma"
	snowball "github.com/GizClaw/flowcraft/sdk/text/stem/adapter/snowball"
	"github.com/GizClaw/flowcraft/sdk/text/stopword"
)

// Simple is the default English / Latin tokenizer. It splits on
// Unicode letter / digit boundaries, lower-cases, filters English
// stop words and tokens shorter than 2 characters, and folds each
// survivor through Lemmatize + a Porter-family stemmer so
// irregular ("went"/"go") and regular ("attending"/"attend") forms
// collapse to one BM25 vocabulary key.
//
// Stemmer is the morphology-folding back-end. Nil falls back to
// Porter2 (Snowball English) — the modern default used by Lucene,
// Elasticsearch and the wider IR community. Porter2 corrects
// several over-stemming bugs the original Porter algorithm has
// (e.g. "general" / "generic" collapsed by Porter1, kept distinct
// by Porter2). Callers who need the historical Porter1 output for
// BM25 index back-compat can pin
//
//	import "github.com/GizClaw/flowcraft/sdk/text/stem"
//	tok := &tokenize.Simple{Stemmer: stem.Porter}
//
// Note that switching stemmers changes the BM25 vocabulary; any
// persisted CorpusStats index built with one stemmer MUST be
// rebuilt before scoring against the other.
//
// Simple is safe for concurrent use: it carries no mutable state,
// and every dependency it consults (stopword / lemma / stem) is
// read-only.
type Simple struct {
	// Stemmer is the function applied after lemmatisation. Nil
	// falls back to Porter2 via the snowball adapter.
	Stemmer func(string) string
}

// Tokenize implements Tokenizer.
func (t *Simple) Tokenize(text string) []string {
	stem := t.Stemmer
	if stem == nil {
		stem = snowball.Stem
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var out []string
	for _, w := range words {
		if len(w) < 2 || stopword.IsEnglish(w) {
			continue
		}
		// Lemmatize first so irregular verb forms (went/bought/taught)
		// and irregular noun plurals (children/feet/mice) collapse to
		// their base form before the stemmer strips regular morphology.
		// Stem alone cannot do this because it operates on suffixes
		// only and irregular forms differ in their stem vowel or are
		// suppletive — see sdk/text/lemma.
		out = append(out, stem(lemma.Lemmatize(w)))
	}
	return out
}
