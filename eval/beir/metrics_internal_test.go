package beir

import (
	"math"
	"testing"
)

// Pure math tests for the IR metrics. We deliberately use closed-form
// inputs so a regression in the formula (wrong log base, wrong gain
// function, off-by-one in the rank loop) lights up immediately,
// independent of any retrieval pipeline.

func approx(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s: want %.6f, got %.6f", label, want, got)
	}
}

// dcg uses the BEIR-standard exponential gain (2^grade - 1) and base-2
// log positional discount.
//
//	dcg([3, 2, 0], 3)
//	  rank 1: (2^3 - 1)/log2(2) = 7/1 = 7
//	  rank 2: (2^2 - 1)/log2(3) ≈ 3/1.585 ≈ 1.893
//	  rank 3: 0/log2(4) = 0
//	  total  ≈ 8.893
func TestDCG_Closed(t *testing.T) {
	got := dcg([]int{3, 2, 0}, 3)
	want := 7.0 + 3.0/math.Log2(3)
	approx(t, "dcg([3,2,0],3)", got, want)

	if got := dcg([]int{}, 5); got != 0 {
		t.Errorf("dcg(empty)=%.3f want 0", got)
	}
	if got := dcg([]int{0, 0, 0}, 3); got != 0 {
		t.Errorf("dcg(all-zero)=%.3f want 0", got)
	}

	// k longer than the ranking should be capped by len(ranking).
	if got := dcg([]int{2}, 100); math.Abs(got-3.0) > 1e-6 {
		t.Errorf("dcg([2],100)=%.6f want 3.0", got)
	}
}

// nDCG normalises against the ideal ranking. A perfect ranking yields 1.
func TestNDCG_Perfect(t *testing.T) {
	approx(t, "perfect", nDCG([]int{2, 1}, []int{2, 1}, 10), 1)
}

// A reversed ranking is below the ideal but non-zero.
func TestNDCG_Reversed(t *testing.T) {
	got := nDCG([]int{1, 2}, []int{2, 1}, 10)
	// ranked dcg = (2^1-1)/1 + (2^2-1)/log2(3) = 1 + 3/log2(3)
	// ideal  dcg = 3 + 1/log2(3)
	ranked := 1.0 + 3.0/math.Log2(3)
	ideal := 3.0 + 1.0/math.Log2(3)
	approx(t, "reversed", got, ranked/ideal)
}

// nDCG must be 0 when the ranking contains nothing relevant.
func TestNDCG_AllIrrelevant(t *testing.T) {
	approx(t, "irrelevant", nDCG([]int{0, 0, 0}, []int{2, 1}, 10), 0)
}

// Recall is the fraction of the totalRel relevant docs caught inside
// the top-k window. Verify both the k > len(ranked) and k < totalRel
// edge cases.
func TestRecall_Cases(t *testing.T) {
	// 2 hits in top-10, 3 total relevant → 2/3.
	approx(t, "partial", recall([]int{1, 0, 2, 0, 0, 0, 0, 0, 0, 0}, 3, 10), 2.0/3.0)
	// k truncates: only 1 hit in top-2 of [1,0,2]; totalRel=2 → 0.5.
	approx(t, "trunc-k", recall([]int{1, 0, 2}, 2, 2), 0.5)
	// No relevant: undefined (we return 0 to match BEIR's pytrec_eval).
	approx(t, "no-rel", recall([]int{0, 0}, 0, 10), 0)
}

// MRR returns the reciprocal of the first relevant doc's 1-indexed
// rank. Zero when no relevant doc is found.
func TestMRR_Cases(t *testing.T) {
	approx(t, "first-hit", mrr([]int{2, 0, 0}), 1.0)
	approx(t, "second-hit", mrr([]int{0, 1, 0}), 0.5)
	approx(t, "fourth-hit", mrr([]int{0, 0, 0, 2}), 0.25)
	approx(t, "no-hit", mrr([]int{0, 0, 0}), 0)
	approx(t, "empty", mrr(nil), 0)
}
