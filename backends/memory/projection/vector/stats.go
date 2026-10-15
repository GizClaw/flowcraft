package vector

import (
	"sync/atomic"
	"time"
)

// Stats is a point-in-time reading of one Index: what the lane has served, and
// which vector tables it currently holds resident. It exists for diagnostics --
// a harness reads it to attribute latency inside the lane and to size the
// cache -- and reading it never changes what the lane serves.
//
// Serving-path counters and warm counters are apart on purpose. A background
// warm pays the same read a query would, but a reader attributing a query's
// latency must not see the warm's time as the query's: a write (and a replay
// of state a store already holds) starts one warm per scope, and the lane
// serves the queries between them.
type Stats struct {
	Queries        int64 `json:"queries"`
	SnapshotHits   int64 `json:"snapshot_hits"`
	SnapshotMisses int64 `json:"snapshot_misses"`
	// SnapshotErrors counts the lookups that returned an error instead of a
	// table. A lookup that failed is neither a hit nor a load: the lane went
	// looking for a table and came back without one, which a reader must not
	// read as "the scope holds nothing" -- a scope that was never indexed
	// reports storage.ErrNotFound here, and a state that cannot be read
	// reports why.
	SnapshotErrors int64         `json:"snapshot_errors"`
	SnapshotLoad   time.Duration `json:"snapshot_load_ns"`
	WarmHits       int64         `json:"warm_hits"`
	WarmMisses     int64         `json:"warm_misses"`
	// WarmErrors counts the background warms that returned an error. A warm
	// that failed is why a later query pays a load of its own, so it is
	// counted rather than dropped.
	WarmErrors     int64         `json:"warm_errors"`
	WarmLoad       time.Duration `json:"warm_load_ns"`
	Embeds         int64         `json:"embeds"`
	EmbedTotal     time.Duration `json:"embed_ns"`
	Scans          int64         `json:"scans"`
	ScanTotal      time.Duration `json:"scan_ns"`
	ScoreTotal     time.Duration `json:"score_ns"`
	SortTotal      time.Duration `json:"sort_ns"`
	EntriesScanned int64         `json:"entries_scanned"`
	EntriesScored  int64         `json:"entries_scored"`
	Footprint      Footprint     `json:"footprint"`
}

// Footprint is the resident state of the lane: one materialized vector table
// per scope read or written since the last invalidation. VectorBytes counts
// the vector payload alone (dimensions times four bytes per entry); entry
// addresses, metadata, and text sit on top of it and are not counted here.
type Footprint struct {
	Scopes      int   `json:"scopes"`
	Entries     int   `json:"entries"`
	Dimensions  int   `json:"dimensions"`
	VectorBytes int64 `json:"vector_bytes"`
}

// statsState is the counter block an Index mutates while serving. One write
// per recorded phase keeps the serving path free of shared locks beyond the
// atomics themselves.
type statsState struct {
	queries      atomic.Int64
	snapshotHits atomic.Int64
	snapshotMiss atomic.Int64
	snapshotErr  atomic.Int64
	loadNanos    atomic.Int64
	warmHits     atomic.Int64
	warmMiss     atomic.Int64
	warmErr      atomic.Int64
	warmNanos    atomic.Int64
	embeds       atomic.Int64
	embedNanos   atomic.Int64
	scans        atomic.Int64
	scanNanos    atomic.Int64
	scoreNanos   atomic.Int64
	sortNanos    atomic.Int64
	scanned      atomic.Int64
	scored       atomic.Int64
}

func (state *statsState) recordQuery() { state.queries.Add(1) }

// recordSnapshotLoad adds one table lookup a query paid. loaded is true when
// the table was materialized off the store instead of served from the cache;
// failed is true when the lookup returned an error, which is counted apart
// from both.
func (state *statsState) recordSnapshotLoad(elapsed time.Duration, loaded, failed bool) {
	state.loadNanos.Add(int64(elapsed))
	switch {
	case failed:
		state.snapshotErr.Add(1)
	case loaded:
		state.snapshotMiss.Add(1)
	default:
		state.snapshotHits.Add(1)
	}
}

// recordWarmLoad adds one background warm. It stays out of the serving-path
// counters above so that a query's own load is what those report.
func (state *statsState) recordWarmLoad(elapsed time.Duration, loaded, failed bool) {
	state.warmNanos.Add(int64(elapsed))
	switch {
	case failed:
		state.warmErr.Add(1)
	case loaded:
		state.warmMiss.Add(1)
	default:
		state.warmHits.Add(1)
	}
}

func (state *statsState) recordEmbed(elapsed time.Duration) {
	state.embeds.Add(1)
	state.embedNanos.Add(int64(elapsed))
}

func (state *statsState) recordScan(report scanReport) {
	state.scans.Add(1)
	state.scanNanos.Add(int64(report.scanNanos))
	state.scoreNanos.Add(int64(report.scoreNanos))
	state.sortNanos.Add(int64(report.sortNanos))
	state.scanned.Add(int64(report.scanned))
	state.scored.Add(int64(report.scored))
}

func (index *Index) recordQuery() {
	if index != nil && index.stats != nil {
		index.stats.recordQuery()
	}
}

func (index *Index) recordSnapshotLoad(elapsed time.Duration, loaded, failed bool) {
	if index != nil && index.stats != nil {
		index.stats.recordSnapshotLoad(elapsed, loaded, failed)
	}
}

func (index *Index) recordWarmLoad(elapsed time.Duration, loaded, failed bool) {
	if index != nil && index.stats != nil {
		index.stats.recordWarmLoad(elapsed, loaded, failed)
	}
}

func (index *Index) recordEmbed(elapsed time.Duration) {
	if index != nil && index.stats != nil {
		index.stats.recordEmbed(elapsed)
	}
}

func (index *Index) recordScan(report scanReport) {
	if index != nil && index.stats != nil {
		index.stats.recordScan(report)
	}
}

// Stats returns the lane's cumulative counters and the tables it holds
// resident. The counters describe this process: they start at zero and only
// grow while the lane serves.
func (index *Index) Stats() Stats {
	if index == nil {
		return Stats{}
	}
	out := Stats{Footprint: index.footprint()}
	state := index.stats
	if state == nil {
		return out
	}
	out.Queries = state.queries.Load()
	out.SnapshotHits = state.snapshotHits.Load()
	out.SnapshotMisses = state.snapshotMiss.Load()
	out.SnapshotErrors = state.snapshotErr.Load()
	out.SnapshotLoad = time.Duration(state.loadNanos.Load())
	out.WarmHits = state.warmHits.Load()
	out.WarmMisses = state.warmMiss.Load()
	out.WarmErrors = state.warmErr.Load()
	out.WarmLoad = time.Duration(state.warmNanos.Load())
	out.Embeds = state.embeds.Load()
	out.EmbedTotal = time.Duration(state.embedNanos.Load())
	out.Scans = state.scans.Load()
	out.ScanTotal = time.Duration(state.scanNanos.Load())
	out.ScoreTotal = time.Duration(state.scoreNanos.Load())
	out.SortTotal = time.Duration(state.sortNanos.Load())
	out.EntriesScanned = state.scanned.Load()
	out.EntriesScored = state.scored.Load()
	return out
}

// footprint sizes the materialized tables the cache holds. Dimensions reports
// the largest table's width, which is the width of every table a lane serves:
// one index is bound to one embedding model.
func (index *Index) footprint() Footprint {
	index.snapshotMu.RLock()
	defer index.snapshotMu.RUnlock()
	out := Footprint{Scopes: len(index.snapshotCache)}
	for _, build := range index.snapshotCache {
		out.Entries += len(build.Entries)
		if build.Dimensions > out.Dimensions {
			out.Dimensions = build.Dimensions
		}
		out.VectorBytes += int64(len(build.Entries)) * int64(build.Dimensions) * 4
	}
	return out
}
