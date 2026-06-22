// Package bm25 implements Okapi BM25 scoring over pre-tokenized text.
//
// BM25 is the canonical lexical retrieval scoring function used
// across the SDK: corpus statistics live in [CorpusStats], and
// single-document scoring goes through [Score]. The k1 / b parameters
// default to the textbook 1.2 / 0.75 and can be overridden per call
// with [WithK1] / [WithB].
//
// CorpusStats is not safe for concurrent mutation. Indexing
// pipelines should synchronise [CorpusStats.AddDocument] /
// [CorpusStats.RemoveDocument] calls externally.
package bm25

import (
	"math"
)

// Score computes the BM25 score for a document (already tokenized)
// against the query keywords.
//
// Callers should analyze content and queries in the analysis package, then pass
// the resulting term slices here.
func Score(docTokens, queryKeywords []string, corpus *CorpusStats, opts ...ScoreOption) float64 {
	if len(docTokens) == 0 || len(queryKeywords) == 0 || corpus == nil || corpus.DocCount == 0 {
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
