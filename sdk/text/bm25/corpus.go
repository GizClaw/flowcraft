package bm25

// CorpusStats holds corpus-level statistics for BM25 scoring:
// document count, average document length, and per-term document
// frequency. It backs every Score / ScoreText call — callers
// instantiate it once per index, feed it via [CorpusStats.AddDocument]
// during ingest, and pass it by pointer to the scoring functions.
//
// CorpusStats is NOT safe for concurrent mutation. Indexing
// pipelines must synchronise [CorpusStats.AddDocument] /
// [CorpusStats.RemoveDocument] calls externally — typically with
// a per-index sync.Mutex that wraps the indexer's write path.
type CorpusStats struct {
	DocCount  int
	AvgLength float64
	DocFreq   map[string]int // term -> number of docs containing it
}

// NewCorpus creates an empty [CorpusStats]. The name is intentionally
// shorter than the legacy textsearch.NewCorpusStats — callers
// typically write bm25.NewCorpus() which reads cleanly without
// the redundant "Stats" suffix that the package path already
// implies.
func NewCorpus() *CorpusStats {
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
