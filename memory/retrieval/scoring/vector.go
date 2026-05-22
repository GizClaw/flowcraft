package scoring

import "math"

// CosineSim returns cosine similarity in [-1, 1] for two equal-length
// float32 vectors. It returns 0 when the vectors differ in length,
// are empty, or when either has zero magnitude (the mathematically
// undefined cases that callers usually want collapsed to "no match").
func CosineSim(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// DotProduct returns the inner product of two equal-length float32
// vectors, or 0 on length mismatch / empty input.
func DotProduct(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// EuclideanDist returns the L2 distance between two equal-length
// float32 vectors. Returns +Inf on length mismatch so callers can
// detect schema drift without conflating it with a real distance.
func EuclideanDist(a, b []float32) float64 {
	if len(a) != len(b) {
		return math.Inf(1)
	}
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return math.Sqrt(s)
}
