package knowledge

import (
	"fmt"
	"math"
)

// CosineSimilarity returns the cosine similarity of two equal-length vectors.
// Returns 0 when lengths differ, either vector is empty, or either vector
// has zero norm. Exported so backends can share one canonical implementation.
func CosineSimilarity(a, b []float32) float64 { return cosineSimilarity(a, b) }

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

func rrfKey(r SearchResult) string {
	return fmt.Sprintf("%s|%d", r.DocName, r.ChunkIndex)
}

func RRFMerge(bm25Results, semanticResults []SearchResult, k int) []SearchResult {
	if k <= 0 {
		k = 60
	}

	type scored struct {
		result SearchResult
		rrf    float64
	}

	merged := make(map[string]*scored)

	for rank, r := range bm25Results {
		key := rrfKey(r)
		if s, ok := merged[key]; ok {
			s.rrf += 1.0 / float64(rank+k)
		} else {
			merged[key] = &scored{result: r, rrf: 1.0 / float64(rank+k)}
		}
	}

	for rank, r := range semanticResults {
		key := rrfKey(r)
		if s, ok := merged[key]; ok {
			s.rrf += 1.0 / float64(rank+k)
		} else {
			merged[key] = &scored{result: r, rrf: 1.0 / float64(rank+k)}
		}
	}

	results := make([]SearchResult, 0, len(merged))
	for _, s := range merged {
		s.result.Score = s.rrf
		results = append(results, s.result)
	}
	return results
}
