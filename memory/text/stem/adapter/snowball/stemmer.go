// Package snowball adapts [github.com/kljensen/snowball] to the
// sdk/text/stem function signature.
//
// snowball implements the Porter2 (Snowball English) stemmer plus
// stemmers for Spanish, French, Russian, Swedish, Norwegian, and
// Hungarian. Porter2 is the modern successor to the original
// Porter algorithm and is the de-facto baseline used by Lucene,
// Elasticsearch, and most production search stacks. It corrects
// several over-stemming bugs the original Porter algorithm has
// (e.g. "general" / "generic" collapsed by Porter1, kept distinct
// by Porter2) and is generally preferred for English BM25 work.
//
// This adapter exposes Porter2 through a function with the same
// signature as [sdk/text/stem.Porter] so callers can swap by
// import path:
//
//	import "github.com/GizClaw/flowcraft/memory/text/stem"            // Porter1
//	import snowball "github.com/GizClaw/flowcraft/memory/text/stem/adapter/snowball"   // Porter2
//
// Switching stemmers changes the BM25 vocabulary; indexes built
// with one MUST be rebuilt before scoring against the other. The
// SDK does not enforce this — callers own the choice and the
// migration story.
package snowball

import (
	"github.com/kljensen/snowball"
)

// Stem returns the Porter2 stem of word using the English
// snowball algorithm. The input is lower-cased internally so
// callers may pass surface forms.
//
// Stop words are not skipped — Stem follows the same contract as
// [sdk/text/stem.Porter]: the input is returned unchanged for
// strings the algorithm cannot reduce further (including very
// short inputs). Callers who want stop-word filtering should
// consult [sdk/text/stopword] first.
//
// Errors from the underlying snowball implementation are
// suppressed: the algorithm returns errors only for unsupported
// languages, and we hard-pin to English. On the off chance the
// upstream library evolves to return a runtime error, we
// degrade gracefully by returning the input unchanged so callers
// never see a half-stemmed token.
func Stem(word string) string {
	out, err := snowball.Stem(word, "english", false)
	if err != nil {
		return word
	}
	return out
}

// StemLang is the explicit-language variant of [Stem]. Use this
// when you need to stem text in one of snowball's supported
// non-English languages (spanish, french, russian, swedish,
// norwegian, hungarian).
//
// Unknown language names return the input unchanged and surface
// the upstream error so callers can detect mis-configuration at
// startup rather than silently returning un-stemmed text. The
// stemStopWords flag is forwarded verbatim: pass true to also
// stem stop words (the default in most search stacks), false to
// preserve them as-is.
func StemLang(word, lang string, stemStopWords bool) (string, error) {
	return snowball.Stem(word, lang, stemStopWords)
}
