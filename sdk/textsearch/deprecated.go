// Package textsearch provides lightweight BM25 text search primitives.
//
// Deprecated: textsearch has been superseded by [sdk/text] and its
// focused sub-packages (text/tokenize, text/stopword, text/stem,
// text/lemma, text/bm25). The package will be removed in v0.5.0;
// migrate to the corresponding text/* sub-package. Every symbol
// here is a thin alias / wrapper that forwards to text/*; no
// behaviour change is intended.
package textsearch

import (
	"github.com/GizClaw/flowcraft/sdk/text/bm25"
	"github.com/GizClaw/flowcraft/sdk/text/lemma"
	"github.com/GizClaw/flowcraft/sdk/text/stem"
	"github.com/GizClaw/flowcraft/sdk/text/stopword"
	"github.com/GizClaw/flowcraft/sdk/text/tokenize"
)

// Tokenizer is an alias for [tokenize.Tokenizer] kept here for the
// v0.5.0 deprecation window.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/tokenize.Tokenizer]. Removed in v0.5.0.
type Tokenizer = tokenize.Tokenizer

// SimpleTokenizer is an alias for [tokenize.Simple] kept here for
// the v0.5.0 deprecation window.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/tokenize.Simple]. Removed in v0.5.0.
type SimpleTokenizer = tokenize.Simple

// CJKTokenizer is an alias for [tokenize.CJKBigram] kept here for
// the v0.5.0 deprecation window.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/tokenize.CJKBigram]. Removed in v0.5.0.
type CJKTokenizer = tokenize.CJKBigram

// CorpusStats is an alias for [bm25.CorpusStats] kept here for the
// v0.5.0 deprecation window.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/bm25.CorpusStats]. Removed in v0.5.0.
type CorpusStats = bm25.CorpusStats

// ScoreOption is an alias for [bm25.ScoreOption] kept here for the
// v0.5.0 deprecation window.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/bm25.ScoreOption]. Removed in v0.5.0.
type ScoreOption = bm25.ScoreOption

// IsCJK reports whether r is a CJK character.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/tokenize.IsCJK]. Removed in v0.5.0.
func IsCJK(r rune) bool { return tokenize.IsCJK(r) }

// DetectTokenizer returns a CJK or Simple tokenizer based on the
// script of sampleText.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/tokenize.Detect]. Removed in v0.5.0.
func DetectTokenizer(sampleText string) Tokenizer { return tokenize.Detect(sampleText) }

// IsStopWord reports whether word is in the English stop-word
// baseline.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/stopword.IsEnglish]. Removed in v0.5.0.
func IsStopWord(word string) bool { return stopword.IsEnglish(word) }

// IsCJKStopChar reports whether r is in the CJK stop-character
// baseline.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/stopword.IsCJKChar]. Removed in v0.5.0.
func IsCJKStopChar(r rune) bool { return stopword.IsCJKChar(r) }

// Stem applies the Porter (1980) stemmer to word.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/stem.Porter]. Removed in v0.5.0.
func Stem(word string) string { return stem.Porter(word) }

// Lemmatize normalises an English irregular inflection to its base
// form.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/lemma.Lemmatize]. Removed in v0.5.0.
func Lemmatize(word string) string { return lemma.Lemmatize(word) }

// NewCorpusStats returns an empty [CorpusStats].
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/bm25.NewCorpus]. Removed in v0.5.0.
func NewCorpusStats() *CorpusStats { return bm25.NewCorpus() }

// BM25 scores docTokens against queryKeywords using the supplied
// corpus statistics.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/bm25.Score]. Removed in v0.5.0.
func BM25(docTokens, queryKeywords []string, corpus *CorpusStats, opts ...ScoreOption) float64 {
	return bm25.Score(docTokens, queryKeywords, corpus, opts...)
}

// ScoreText tokenizes text first, then scores it against keywords.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/bm25.ScoreText]. Removed in v0.5.0.
func ScoreText(text string, keywords []string, corpus *CorpusStats, tokenizer Tokenizer, opts ...ScoreOption) float64 {
	return bm25.ScoreText(text, keywords, corpus, tokenizer, opts...)
}

// ExtractKeywords tokenizes text and deduplicates the result.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/bm25.ExtractKeywords]. Removed in v0.5.0.
func ExtractKeywords(text string, tokenizer Tokenizer) []string {
	return bm25.ExtractKeywords(text, tokenizer)
}

// WithK1 sets the BM25 term frequency saturation parameter.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/bm25.WithK1]. Removed in v0.5.0.
func WithK1(k1 float64) ScoreOption { return bm25.WithK1(k1) }

// WithB sets the BM25 length normalization strength.
//
// Deprecated: use [github.com/GizClaw/flowcraft/sdk/text/bm25.WithB]. Removed in v0.5.0.
func WithB(b float64) ScoreOption { return bm25.WithB(b) }
