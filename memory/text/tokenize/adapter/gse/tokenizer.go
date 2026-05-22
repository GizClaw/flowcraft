// Package gse adapts [github.com/go-ego/gse] to the
// sdk/text/tokenize.Tokenizer interface.
//
// gse is a pure-Go reimplementation of jieba — the de-facto
// Chinese segmentation library. It performs real word-level
// segmentation backed by a double-array trie dictionary plus an
// HMM model for unknown words, in contrast to
// [sdk/text/tokenize.CJKBigram] which emits naive unigrams +
// bigrams.
//
// Choosing between gse and CJKBigram is a precision / dependency
// trade-off:
//
//   - CJKBigram (default): zero dependencies, ~10 lines of code,
//     decent recall, terrible precision (every two adjacent
//     characters become a token). Indexes inflate by ~2× but no
//     query word is missed.
//   - gse (this adapter): ~5 MB embedded Chinese dictionary,
//     ~10 MB binary overhead, real word boundaries, dramatically
//     higher precision. Use for product workloads where
//     Chinese-language results quality matters.
//
// gse cannot be swapped at index-rebuild time without re-tokenising
// every document; index vocabularies diverge. The SDK does not
// enforce a rebuild — callers own the migration story.
package gse

import (
	"strings"

	"github.com/go-ego/gse"
)

// Tokenizer wraps a [gse.Segmenter] and exposes it through the
// sdk/text/tokenize.Tokenizer interface.
//
// Each Tokenizer owns its own Segmenter. The Segmenter is
// expensive to construct (loads a multi-megabyte dictionary),
// so callers must construct one Tokenizer at process startup
// and share it across goroutines. The Segmenter is safe for
// concurrent reads after construction.
type Tokenizer struct {
	seg gse.Segmenter
	hmm bool
}

// Option configures a [Tokenizer] at construction time.
type Option func(*config)

type config struct {
	dictSpec string
	hmm      bool
}

// WithDict overrides the gse dictionary specification (passed to
// [gse.Segmenter.LoadDictEmbed]). The default is "zh" which loads
// the embedded Simplified Chinese dictionary. Other supported
// values include "zh_t" (Traditional Chinese), "jp" (Japanese),
// and comma-separated combinations. Empty falls back to "zh".
func WithDict(spec string) Option {
	return func(c *config) { c.dictSpec = spec }
}

// WithHMM toggles HMM unknown-word resolution. The default is
// enabled — HMM handles tokens that are missing from the
// dictionary (proper nouns, neologisms, transliterations) and
// improves recall on conversational Chinese text. Disable only
// for closed-vocabulary workloads where the dictionary is
// authoritative.
//
// Case folding for ASCII runs is NOT configurable: gse's
// lower-casing is controlled by a package-level variable
// ([gse.ToLower]) shared across all Segmenter instances in the
// process. Mutating it from this adapter would be a footgun for
// callers that hold their own Segmenter elsewhere. The output is
// always lower-cased for BM25 vocabulary consistency with
// sdk/text/tokenize.Simple; callers needing raw surface forms
// should reach for [sdk/text/tokenize.SplitWords] instead.
func WithHMM(enabled bool) Option {
	return func(c *config) { c.hmm = enabled }
}

// New constructs a [Tokenizer] backed by the embedded gse Chinese
// dictionary. Returns an error if the embedded dictionary fails
// to load — typically only on a corrupted module download.
//
// Construction takes O(100ms) because the dictionary is parsed
// into a double-array trie. Callers must reuse the returned
// Tokenizer rather than constructing one per request.
func New(opts ...Option) (*Tokenizer, error) {
	cfg := config{dictSpec: "zh", hmm: true}
	for _, o := range opts {
		o(&cfg)
	}
	t := &Tokenizer{hmm: cfg.hmm}
	if err := t.seg.LoadDictEmbed(cfg.dictSpec); err != nil {
		return nil, err
	}
	return t, nil
}

// Tokenize implements [sdk/text/tokenize.Tokenizer].
//
// Internally calls [gse.Segmenter.CutSearch], which is the
// search-engine-optimised cut mode — it emits both the shortest-
// path segmentation AND alternative sub-segments so a BM25 index
// retrieves on every reasonable substring of a multi-character
// term. CutSearch is more recall-friendly than Cut at the cost
// of a slightly larger vocabulary.
//
// ASCII case folding is applied at the input boundary so the
// output matches sdk/text/tokenize.Simple's lower-cased
// vocabulary. Empty tokens and pure-whitespace splits are
// filtered out.
func (t *Tokenizer) Tokenize(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.ToLower(text)
	raw := t.seg.CutSearch(text, t.hmm)
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		out = append(out, tok)
	}
	return out
}
