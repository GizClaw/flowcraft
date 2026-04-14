package textsearch

import "math"

// CorpusStats holds corpus-level statistics for BM25 scoring.
// It is not safe for concurrent use; callers must synchronize access externally.
type CorpusStats struct {
	DocCount  int
	AvgLength float64
	DocFreq   map[string]int // term -> number of docs containing it
}

// NewCorpusStats creates an empty CorpusStats.
func NewCorpusStats() *CorpusStats {
	return &CorpusStats{DocFreq: make(map[string]int)}
}

// AddDocument updates corpus statistics for a new document's tokens.
func (cs *CorpusStats) AddDocument(tokens []string) {
	cs.DocCount++
	seen := make(map[string]bool, len(tokens))
	totalLen := cs.AvgLength * float64(cs.DocCount-1)
	totalLen += float64(len(tokens))
	cs.AvgLength = totalLen / float64(cs.DocCount)
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			cs.DocFreq[t]++
		}
	}
}

// RemoveDocument updates corpus statistics when a document is removed.
func (cs *CorpusStats) RemoveDocument(tokens []string) {
	if cs.DocCount <= 0 {
		return
	}
	totalLen := cs.AvgLength * float64(cs.DocCount)
	totalLen -= float64(len(tokens))
	cs.DocCount--
	if cs.DocCount > 0 {
		cs.AvgLength = totalLen / float64(cs.DocCount)
	} else {
		cs.AvgLength = 0
	}
	seen := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			if cs.DocFreq[t] > 0 {
				cs.DocFreq[t]--
				if cs.DocFreq[t] == 0 {
					delete(cs.DocFreq, t)
				}
			}
		}
	}
}

// ScoreText computes BM25 for arbitrary text content.
func ScoreText(text string, keywords []string, corpus *CorpusStats, tokenizer Tokenizer, opts ...ScoreOption) float64 {
	if corpus == nil || corpus.DocCount == 0 || len(keywords) == 0 {
		return 0
	}
	tokens := tokenizer.Tokenize(text)
	return BM25(tokens, keywords, corpus, opts...)
}

// ExtractKeywords tokenizes text and deduplicates.
func ExtractKeywords(text string, tokenizer Tokenizer) []string {
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

type scoreConfig struct {
	k1, b float64
}

// ScoreOption configures BM25 scoring parameters.
type ScoreOption func(*scoreConfig)

// WithK1 sets the BM25 term frequency saturation parameter (default 1.2).
func WithK1(k1 float64) ScoreOption {
	return func(c *scoreConfig) { c.k1 = k1 }
}

// WithB sets the BM25 length normalization strength (default 0.75).
func WithB(b float64) ScoreOption {
	return func(c *scoreConfig) { c.b = b }
}

func applyScoreOptions(opts []ScoreOption) scoreConfig {
	c := scoreConfig{k1: 1.2, b: 0.75}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// BM25 computes the BM25 score for a document (as tokens) against query keywords.
func BM25(docTokens, queryKeywords []string, corpus *CorpusStats, opts ...ScoreOption) float64 {
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
