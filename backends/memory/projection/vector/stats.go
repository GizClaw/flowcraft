package vector

import (
	"sync/atomic"
	"time"

	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
)

// Stats is a point-in-time reading of one Index: what the lane has served and
// what it paid to read the state it served. It exists for diagnostics -- a
// harness reads it to attribute latency inside the lane -- and reading it never
// changes what the lane serves.
//
// The lane keeps no table of its own: every planned shard is materialized for
// the query that named it and dropped when the query returns, so the only
// resident copy of a table is the store's decoded base. ShardLoads and
// ShardLoadTotal count the materializations a query paid for; a read served
// out of the store's kept base is a load with a cheap duration, not a hit the
// lane can attribute.
type Stats struct {
	Queries int64 `json:"queries"`
	// ShardLoads counts the shard tables a query materialized.
	ShardLoads int64 `json:"shard_loads"`
	// ShardErrors counts the lookups that returned an error instead of a table.
	// A lookup that failed is not a load: the lane went looking for a table and
	// came back without one, which a reader must not read as "the scope holds
	// nothing" -- a shard that was never written reports storage.ErrNotFound
	// here, and a state that cannot be read reports why.
	ShardErrors    int64         `json:"shard_errors"`
	ShardLoadTotal time.Duration `json:"shard_load_ns"`
	Embeds         int64         `json:"embeds"`
	EmbedTotal     time.Duration `json:"embed_ns"`
	Scans          int64         `json:"scans"`
	ScanTotal      time.Duration `json:"scan_ns"`
	ScoreTotal     time.Duration `json:"score_ns"`
	SortTotal      time.Duration `json:"sort_ns"`
	EntriesScanned int64         `json:"entries_scanned"`
	EntriesScored  int64         `json:"entries_scored"`
	// Resident is what the store behind the lane keeps decoded: the number of
	// tables held, the bytes they are reckoned at, and the reads that found
	// them (hits), had to decode them (misses), and paid for the bound
	// (evictions, uncached). It is the lane's footprint, reported by the one
	// component that holds one.
	Resident projectionstore.CacheStats `json:"resident"`
}

// statsState is the counter block an Index mutates while serving. One write
// per recorded phase keeps the serving path free of shared locks beyond the
// atomics themselves.
type statsState struct {
	queries    atomic.Int64
	shardLoads atomic.Int64
	shardErr   atomic.Int64
	loadNanos  atomic.Int64
	embeds     atomic.Int64
	embedNanos atomic.Int64
	scans      atomic.Int64
	scanNanos  atomic.Int64
	scoreNanos atomic.Int64
	sortNanos  atomic.Int64
	scanned    atomic.Int64
	scored     atomic.Int64
}

func (state *statsState) recordQuery() { state.queries.Add(1) }

// recordShardLoad adds one table lookup a query paid; failed is true when the
// lookup returned an error, which is counted apart from a load.
func (state *statsState) recordShardLoad(elapsed time.Duration, failed bool) {
	state.loadNanos.Add(int64(elapsed))
	if failed {
		state.shardErr.Add(1)
		return
	}
	state.shardLoads.Add(1)
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

func (index *Index) recordShardLoad(elapsed time.Duration, failed bool) {
	if index != nil && index.stats != nil {
		index.stats.recordShardLoad(elapsed, failed)
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

// Stats returns the lane's cumulative counters. They describe this process:
// they start at zero and only grow while the lane serves.
func (index *Index) Stats() Stats {
	if index == nil || index.stats == nil {
		return Stats{}
	}
	state := index.stats
	return Stats{
		Queries:        state.queries.Load(),
		ShardLoads:     state.shardLoads.Load(),
		ShardErrors:    state.shardErr.Load(),
		ShardLoadTotal: time.Duration(state.loadNanos.Load()),
		Embeds:         state.embeds.Load(),
		EmbedTotal:     time.Duration(state.embedNanos.Load()),
		Scans:          state.scans.Load(),
		ScanTotal:      time.Duration(state.scanNanos.Load()),
		ScoreTotal:     time.Duration(state.scoreNanos.Load()),
		SortTotal:      time.Duration(state.sortNanos.Load()),
		EntriesScanned: state.scanned.Load(),
		EntriesScored:  state.scored.Load(),
		Resident:       index.store.Resident(),
	}
}
