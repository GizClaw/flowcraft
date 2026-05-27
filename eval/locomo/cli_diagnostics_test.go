package locomo

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
)

func TestToLatencyReports(t *testing.T) {
	var stats diagnostics.LatencyStats
	for _, d := range []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond} {
		stats.Add(d)
	}

	got := toLatencyReports(map[string]diagnostics.LatencyStats{"context_pack": stats})
	report, ok := got["context_pack"]
	if !ok {
		t.Fatalf("missing context_pack report: %+v", got)
	}
	if report.Count != 3 || report.Avg != "20ms" || report.P50 != "20ms" || report.P95 != "30ms" || report.Max != "30ms" {
		t.Fatalf("latency report = %+v", report)
	}
}
