package fleet

import (
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/vessel"
)

// VesselSnapshot is the per-vessel projection consumed by /metrics
// (and convenient for /healthz aggregation later). It is computed on
// demand at scrape time so the gauges always reflect "now" rather
// than a possibly-stale watcher's last update.
type VesselSnapshot struct {
	Name           string
	Phase          vessel.Phase
	RunsInflight   int
	RunsByState    map[string]int64 // keyed by RunStatus.State; only terminal states are summed.
	DurationSumSec float64
	DurationCount  int64
}

// Snapshot returns one VesselSnapshot per registered captain in
// stable plan order. It walks the run registry exactly once and
// fans the buckets out per vessel — the alternative (one full
// registry scan per vessel) would be O(N*M) on the cold path.
//
// Inflight is the count of registry entries with a zero
// CompletedAt for that vessel. Terminal states bump RunsByState
// keyed on the same string the HTTP /v1/runs/{id} endpoint reports,
// so /metrics and the JSON API agree.
func (f *Fleet) Snapshot() []VesselSnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()

	out := make(map[string]*VesselSnapshot, len(f.captains))
	names := make([]string, 0, len(f.captains))
	for _, vp := range f.plan.Vessels {
		ent, ok := f.captains[vp.Name]
		if !ok {
			continue
		}
		out[vp.Name] = &VesselSnapshot{
			Name:        vp.Name,
			Phase:       ent.cap.Phase(),
			RunsByState: map[string]int64{},
		}
		names = append(names, vp.Name)
	}

	if f.runs != nil {
		f.runs.mu.RLock()
		for _, e := range f.runs.runs {
			snap, ok := out[e.VesselName]
			if !ok {
				continue
			}
			if e.CompletedAt.IsZero() {
				snap.RunsInflight++
				continue
			}
			state := terminalStateLabel(e)
			snap.RunsByState[state]++
			snap.DurationCount++
			snap.DurationSumSec += e.CompletedAt.Sub(e.StartedAt).Seconds()
		}
		f.runs.mu.RUnlock()
	}

	sort.Strings(names) // /metrics output is sorted by name for stability.
	res := make([]VesselSnapshot, 0, len(names))
	for _, n := range names {
		res = append(res, *out[n])
	}
	return res
}

// terminalStateLabel mirrors the State derivation used by
// LookupRun so /metrics and /v1/runs/{id} report identical labels.
func terminalStateLabel(e *runEntry) string {
	if e.Err != nil {
		return "error"
	}
	if e.Status != "" {
		return string(e.Status)
	}
	return "completed"
}

// Avoid an unused-import lint when nobody reads this file's helpers.
var _ = time.Time{}
