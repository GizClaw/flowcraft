package diagnostics

import (
	"math"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// LatencyStats aggregates latency samples for one diagnostic dimension.
type LatencyStats struct {
	Count   int
	Total   time.Duration
	Max     time.Duration
	Samples []time.Duration
}

// LatencySummary is the read-only view of LatencyStats with percentiles
// computed from the retained samples.
type LatencySummary struct {
	Count int
	Avg   time.Duration
	P50   time.Duration
	P95   time.Duration
	Max   time.Duration
}

func (s *LatencyStats) Add(d time.Duration) {
	if d < 0 {
		return
	}
	s.Count++
	s.Total += d
	if d > s.Max {
		s.Max = d
	}
	s.Samples = append(s.Samples, d)
}

func (s LatencyStats) Summary() LatencySummary {
	if s.Count == 0 {
		return LatencySummary{}
	}
	samples := append([]time.Duration(nil), s.Samples...)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return LatencySummary{
		Count: s.Count,
		Avg:   s.Total / time.Duration(s.Count),
		P50:   percentileDuration(samples, 0.50),
		P95:   percentileDuration(samples, 0.95),
		Max:   s.Max,
	}
}

func stageLatencies(stages []diagnostic.StageDiagnostic) map[string]time.Duration {
	out := map[string]time.Duration{}
	for _, st := range stages {
		if st.Stage == "" {
			continue
		}
		out[st.Stage] += st.Duration
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeLatencySample(dst map[string]LatencyStats, key string, d time.Duration) {
	if key == "" {
		return
	}
	stats := dst[key]
	stats.Add(d)
	dst[key] = stats
}

func percentileDuration(sorted []time.Duration, pct float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if pct <= 0 {
		return sorted[0]
	}
	if pct >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(float64(len(sorted))*pct)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
