package metrics

import (
	"sort"
	"time"
)

// LatencySummary holds P50/P95/P99 and mean for a sample.
type LatencySummary struct {
	N    int           `json:"n"`
	Mean time.Duration `json:"mean"`
	P50  time.Duration `json:"p50"`
	P95  time.Duration `json:"p95"`
	P99  time.Duration `json:"p99"`
}

// Summarize returns the percentile breakdown for the slice (mutates copy).
func Summarize(samples []time.Duration) LatencySummary {
	if len(samples) == 0 {
		return LatencySummary{}
	}
	cp := append([]time.Duration(nil), samples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	var sum time.Duration
	for _, d := range cp {
		sum += d
	}
	return LatencySummary{
		N:    len(cp),
		Mean: sum / time.Duration(len(cp)),
		P50:  cp[pIdx(len(cp), 0.50)],
		P95:  cp[pIdx(len(cp), 0.95)],
		P99:  cp[pIdx(len(cp), 0.99)],
	}
}

func pIdx(n int, p float64) int {
	idx := int(float64(n-1) * p)
	if idx >= n {
		idx = n - 1
	}
	return idx
}
