package knowledge

import "math"

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
