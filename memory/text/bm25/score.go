// Package bm25 implements Okapi BM25 scoring with a pluggable
// tokenizer.
//
// BM25 is the canonical lexical retrieval scoring function used
// across the SDK: corpus statistics live in [CorpusStats], single-
// document scoring goes through [Score], and free-text scoring
// (tokenize on the fly) goes through [ScoreText]. The k1 / b
// parameters default to the textbook 1.2 / 0.75 and can be
// overridden per call with [WithK1] / [WithB].
//
// CorpusStats is not safe for concurrent mutation. Indexing
// pipelines should synchronise [CorpusStats.AddDocument] /
// [CorpusStats.RemoveDocument] calls externally.
package bm25

import (
	"math"

	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// Score computes the BM25 score for a document (already tokenized)
// against the query keywords.
//
// This is the core scoring primitive — callers that already hold a
// tokenized document slice should reach for [Score] directly;
// [ScoreText] is sugar that tokenizes the input first.
func Score(docTokens, queryKeywords []string, corpus *CorpusStats, opts ...ScoreOption) float64 {
	if len(docTokens) == 0 {
		return 0
	}
	cfg := applyScoreOptions(opts)
	k1, b := cfg.k1, cfg.b
	dl := float64(len(docTokens))
	avgDL := corpus.AvgLength
	if avgDL <= 0 {
		avgDL = dl
	}

	tf := make(map[string]int, len(docTokens))
	for _, t := range docTokens {
		tf[t]++
	}

	var score float64
	for _, kw := range queryKeywords {
		freq := tf[kw]
		if freq == 0 {
			continue
		}
		df := corpus.DocFreq[kw]
		idf := math.Log((float64(corpus.DocCount)-float64(df)+0.5)/(float64(df)+0.5) + 1.0)
		tfNorm := float64(freq) * (k1 + 1) / (float64(freq) + k1*(1-b+b*dl/avgDL))
		score += idf * tfNorm
	}
	return score
}

// ScoreText computes BM25 for arbitrary text content by tokenizing
// it with the supplied [tokenize.Tokenizer] first.
func ScoreText(text string, keywords []string, corpus *CorpusStats, tokenizer tokenize.Tokenizer, opts ...ScoreOption) float64 {
	if corpus == nil || corpus.DocCount == 0 || len(keywords) == 0 {
		return 0
	}
	tokens := tokenizer.Tokenize(text)
	return Score(tokens, keywords, corpus, opts...)
}

// ExtractKeywords tokenizes text and deduplicates the result,
// returning a stable-ordered slice suitable as a BM25 query.
func ExtractKeywords(text string, tokenizer tokenize.Tokenizer) []string {
	tokens := tokenizer.Tokenize(text)
	seen := make(map[string]bool, len(tokens))
	var unique []string
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}
	return unique
}
