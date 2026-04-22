package knowledge

import (
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/textsearch"
)

// Type and function aliases re-exported from sdk/textsearch for backward
// compatibility. Internal code and tests can use these without changing
// import paths.
type (
	Tokenizer       = textsearch.Tokenizer
	SimpleTokenizer = textsearch.SimpleTokenizer
	CJKTokenizer    = textsearch.CJKTokenizer
	CorpusStats     = textsearch.CorpusStats
)

var (
	DetectTokenizer = textsearch.DetectTokenizer
	NewCorpusStats  = textsearch.NewCorpusStats
	ExtractKeywords = textsearch.ExtractKeywords
	ScoreText       = textsearch.ScoreText
)

// ScoreChunk computes the BM25 score for a chunk against query keywords.
func ScoreChunk(chunk *Chunk, keywords []string, corpus *CorpusStats, tokenizer Tokenizer) float64 {
	if corpus == nil || corpus.DocCount == 0 || len(keywords) == 0 {
		return 0
	}
	tokens := tokenizer.Tokenize(chunk.Content)
	return textsearch.BM25(tokens, keywords, corpus)
}

// RankResults sorts by score descending and limits to topK.
func RankResults(results []SearchResult, topK int) []SearchResult {
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results
}

// parseFrontmatter extracts YAML frontmatter (between "---" delimiters).
func parseFrontmatter(raw string) (body string, meta map[string]string) {
	if !strings.HasPrefix(raw, "---\n") {
		return raw, nil
	}
	end := strings.Index(raw[4:], "\n---")
	if end < 0 {
		return raw, nil
	}
	fmBlock := raw[4 : 4+end]
	body = strings.TrimLeft(raw[4+end+4:], "\n")

	meta = make(map[string]string)
	for _, line := range strings.Split(fmBlock, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			meta[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return body, meta
}
