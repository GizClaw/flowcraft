package api

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/cmd/vesseld/fleet"
	"github.com/GizClaw/flowcraft/vessel"
)

// metricsLabelOrder pins label-ordering for stability across scrapes.
// Prometheus does not require this, but it keeps `curl /metrics`
// diffable in tests and operator workflows.
var metricsLabelOrder = []string{"vessel", "phase", "state"}

// allPhases drives the per-vessel phase gauge so we always emit a
// 0 / 1 row for every known phase. Without this, phases that have
// not yet been visited would simply be missing from the scrape and
// dashboards would treat "no series" as "0", which is the Prometheus
// rule but is fragile to grep against in alerts.
var allPhases = []vessel.Phase{
	vessel.PhasePending,
	vessel.PhaseRunning,
	vessel.PhaseDraining,
	vessel.PhaseStopping,
	vessel.PhaseStopped,
	vessel.PhaseFailed,
}

// handleMetrics renders the Prometheus text exposition format
// (version 0.0.4, the dominant variant). Implementing this by hand
// avoids a hard dependency on prometheus/client_golang for what is
// effectively a dozen counters / gauges; if and when we adopt OTel
// metrics this handler is replaced wholesale.
//
// The endpoint is intentionally NOT auth-gated: Prometheus scrapers
// in production live on a private network and cannot easily present
// bearer tokens. Operators that need to lock it down can put the
// daemon behind an authenticating proxy or restrict the unix socket
// permissions.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	snaps := s.fleet.Snapshot()

	// Build info — useful for confirming a deploy without hitting
	// /v1/version (which is auth-gated on TCP).
	writeHelp(w, "vesseld_build_info", "Constant 1 series labelled with the running daemon version.", "gauge")
	writeMetric(w, "vesseld_build_info", map[string]string{"version": s.cfg.Version}, 1)

	// Inflight runs per vessel.
	writeHelp(w, "vesseld_runs_inflight", "Number of runs currently executing on a vessel (registry entries with no completion timestamp).", "gauge")
	for _, snap := range snaps {
		writeMetric(w, "vesseld_runs_inflight",
			map[string]string{"vessel": snap.Name},
			float64(snap.RunsInflight))
	}

	// Total terminated runs by state.
	writeHelp(w, "vesseld_runs_total", "Cumulative count of completed runs partitioned by terminal state.", "counter")
	for _, snap := range snaps {
		states := sortedKeys(snap.RunsByState)
		for _, st := range states {
			writeMetric(w, "vesseld_runs_total",
				map[string]string{"vessel": snap.Name, "state": st},
				float64(snap.RunsByState[st]))
		}
	}

	// Run duration. Exposed as _sum / _count so dashboards can
	// derive average without us having to commit to a histogram
	// bucket schema before we have user signal on what bucket
	// boundaries make sense.
	writeHelp(w, "vesseld_run_duration_seconds_sum", "Sum of wall-clock run durations in seconds (terminated runs only).", "counter")
	writeHelp(w, "vesseld_run_duration_seconds_count", "Number of runs contributing to vesseld_run_duration_seconds_sum.", "counter")
	for _, snap := range snaps {
		labels := map[string]string{"vessel": snap.Name}
		writeMetric(w, "vesseld_run_duration_seconds_sum", labels, snap.DurationSumSec)
		writeMetric(w, "vesseld_run_duration_seconds_count", labels, float64(snap.DurationCount))
	}

	// Per-vessel phase gauge — one row per (vessel, phase) where
	// the row is 1 iff the captain is currently in that phase.
	writeHelp(w, "vesseld_vessel_phase", "1 when the vessel is currently in the labelled phase, else 0.", "gauge")
	for _, snap := range snaps {
		for _, p := range allPhases {
			val := 0.0
			if snap.Phase == p {
				val = 1
			}
			writeMetric(w, "vesseld_vessel_phase",
				map[string]string{"vessel": snap.Name, "phase": string(p)},
				val)
		}
	}
}

// writeHelp emits the standard `# HELP` + `# TYPE` preamble for one
// metric family. Callers are expected to follow it with one
// writeMetric call per series.
func writeHelp(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, escapeHelp(help))
	fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)
}

// writeMetric emits one labelled sample. Label keys are sorted
// according to metricsLabelOrder (then alphabetically) for stable
// output.
func writeMetric(w io.Writer, name string, labels map[string]string, val float64) {
	if len(labels) == 0 {
		fmt.Fprintf(w, "%s %s\n", name, formatFloat(val))
		return
	}
	keys := orderedLabelKeys(labels)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	b.WriteByte(' ')
	b.WriteString(formatFloat(val))
	b.WriteByte('\n')
	_, _ = io.WriteString(w, b.String())
}

func orderedLabelKeys(labels map[string]string) []string {
	out := make([]string, 0, len(labels))
	priority := map[string]int{}
	for i, k := range metricsLabelOrder {
		priority[k] = i
	}
	for k := range labels {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, oki := priority[out[i]]
		pj, okj := priority[out[j]]
		switch {
		case oki && okj:
			return pi < pj
		case oki:
			return true
		case okj:
			return false
		default:
			return out[i] < out[j]
		}
	})
	return out
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func formatFloat(v float64) string {
	// Prometheus accepts both integer and decimal forms. Render
	// integers as integers for compactness; reserve the float
	// form for fractional values.
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// Compile-time assertion: handleMetrics depends on the snapshot
// returning a slice keyed by snapshot.Name (we sort and stamp every
// metric series with that name). If the field is renamed the build
// breaks here rather than producing silent label drift in prod.
var _ = func() string { var s fleet.VesselSnapshot; return s.Name }()
