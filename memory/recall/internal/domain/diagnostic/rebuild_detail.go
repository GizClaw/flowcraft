package diagnostic

import "time"

// ScanDetail —— rebuild/scan stage (scan the temporal store for the
// scope being rebuilt). AfterValidity counts the survivors of the
// active-only filter.
type ScanDetail struct {
	ScopeKey      string
	Total         int
	AfterValidity int
	Latency       time.Duration
}

func (ScanDetail) isStageDetail() {}

// RebuildProjectionDetail —— rebuild/project stage. DriftDetected
// fires when PriorEntries != Applied, surfacing silent projection
// drift to operators.
type RebuildProjectionDetail struct {
	ProjectionName string
	Applied        int
	Dropped        int
	PriorEntries   int
	DriftDetected  bool
	Latency        time.Duration
}

func (RebuildProjectionDetail) isStageDetail() {}

// RebuildGraphDetail —— rebuild/graph_ledger stage. Rebuilds the experimental
// Observation/Assertion/Link ledger from canonical TemporalFacts.
type RebuildGraphDetail struct {
	Observations int
	Links        int
	Latency      time.Duration
}

func (RebuildGraphDetail) isStageDetail() {}
