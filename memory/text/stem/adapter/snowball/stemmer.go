// Package snowball adapts [github.com/kljensen/snowball] to the tokenizer
// stemmer function signature.
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
// This adapter exposes Porter2 through a plain func(string) string so callers
// can pass it directly into tokenizer configuration:
//
//	import snowball "github.com/GizClaw/flowcraft/memory/text/stem/adapter/snowball"
//
// Switching stemmers changes the BM25 vocabulary; indexes built
// with one MUST be rebuilt before scoring against the other. The
// SDK does not enforce this — callers own the choice and the
// migration story.
package snowball

import (
	"strings"

	"github.com/kljensen/snowball"
)

// Stem returns the Porter2 stem of word using the English
// snowball algorithm. The input is lower-cased internally so
// callers may pass surface forms.
//
// Stop words are not skipped: the input is returned unchanged for strings the
// algorithm cannot reduce further (including very short inputs). Callers who
// want stop-word filtering should consult [memory/text/stopword] first.
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
	return snowball.Stem(word, normalizeLanguage(lang), stemStopWords)
}

// StemFirst returns the shortest successful stem from langs that changes word.
// Unsupported languages are skipped so callers can pass a broad preference list
// without making tokenisation fail at runtime. If no language changes word, the
// original input is returned.
func StemFirst(word string, langs ...string) string {
	word = strings.ToLower(word)
	if word == "" {
		return ""
	}
	best := ""
	for _, lang := range langs {
		stemmed, err := snowball.Stem(word, normalizeLanguage(lang), false)
		if err != nil || stemmed == "" {
			continue
		}
		if stemmed != word {
			if best == "" || len([]rune(stemmed)) < len([]rune(best)) {
				best = stemmed
			}
		}
	}
	if best != "" {
		return best
	}
	return word
}

func normalizeLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "en", "english":
		return "english"
	case "es", "spanish":
		return "spanish"
	case "fr", "french":
		return "french"
	case "ru", "russian":
		return "russian"
	case "sv", "swedish":
		return "swedish"
	case "no", "norwegian":
		return "norwegian"
	case "hu", "hungarian":
		return "hungarian"
	default:
		return lang
	}
}
