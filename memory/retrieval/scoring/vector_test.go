package scoring

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestCosineSim(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0}, []float32{1, 0}, 1.0},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0.0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1.0},
		{"empty_a", []float32{}, []float32{}, 0.0},
		{"length_mismatch", []float32{1, 0}, []float32{1, 0, 0}, 0.0},
		{"zero_a", []float32{0, 0}, []float32{1, 1}, 0.0},
		{"zero_b", []float32{1, 1}, []float32{0, 0}, 0.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CosineSim(c.a, c.b)
			if !almostEqual(got, c.want) {
				t.Errorf("CosineSim(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestDotProduct(t *testing.T) {
	got := DotProduct([]float32{1, 2, 3}, []float32{4, 5, 6})
	if !almostEqual(got, 32) {
		t.Errorf("DotProduct = %v, want 32", got)
	}
	if DotProduct([]float32{1}, []float32{1, 2}) != 0 {
		t.Errorf("length mismatch should yield 0")
	}
	if DotProduct(nil, nil) != 0 {
		t.Errorf("empty should yield 0")
	}
}

func TestEuclideanDist(t *testing.T) {
	got := EuclideanDist([]float32{0, 0}, []float32{3, 4})
	if !almostEqual(got, 5) {
		t.Errorf("EuclideanDist = %v, want 5", got)
	}
	if !math.IsInf(EuclideanDist([]float32{1}, []float32{1, 2}), 1) {
		t.Errorf("length mismatch should yield +Inf")
	}
}
