package main

import (
	"fmt"
	"io"
	"sort"
	"time"
)

func renderCold(w io.Writer, readings []coldReading) {
	if len(readings) == 0 {
		return
	}
	fmt.Fprintf(w, "cold pass (%d scopes, the first question of each, sequential)\n", len(readings))
	fmt.Fprintf(w, "  %-14s %10s %10s %7s %10s %7s %8s %10s %10s\n",
		"conversation", "latency", "load", "misses", "warm", "warms", "entries", "vector", "heap+d")
	loads := make([]time.Duration, 0, len(readings))
	warms := make([]time.Duration, 0, len(readings))
	for _, reading := range readings {
		loads = append(loads, reading.SnapshotLoad)
		warms = append(warms, reading.WarmLoad)
		fmt.Fprintf(w, "  %-14s %10s %10s %7d %10s %7d %8d %10s %10s\n",
			reading.ConversationID, duration(reading.Latency), duration(reading.SnapshotLoad),
			reading.SnapshotMisses, duration(reading.WarmLoad), reading.WarmMisses,
			reading.EntriesAdded, megabytes(reading.VectorBytesAdded),
			megabytes(reading.HeapDeltaBytes))
	}
	fmt.Fprintf(w, "  median load %s, median warm %s\n", duration(median(loads)), duration(median(warms)))
	fmt.Fprintf(w, "  note: load is the question's own materialization (misses counts the questions that paid\n")
	fmt.Fprintf(w, "        it); warm is what the deployment's writes -- including the replay of a store it\n")
	fmt.Fprintf(w, "        already holds -- paid beside it, so a first question can find the table resident\n\n")
}

func renderHot(w io.Writer, hot *hotReading) {
	if hot == nil {
		return
	}
	fmt.Fprintf(w, "hot pass (%d questions, %d workers, wall %s)\n", hot.Questions, hot.Workers, duration(hot.Wall))
	fmt.Fprintf(w, "  latency  mean %s  p50 %s  p90 %s  p99 %s  max %s  over-5s %d\n",
		duration(hot.Latency.Mean), duration(hot.Latency.P50), duration(hot.Latency.P90),
		duration(hot.Latency.P99), duration(hot.Latency.Max), hot.Latency.Over5s)
	fmt.Fprintln(w, "  stages (sum over the pass, per-request mean)")
	for _, name := range sortedStages(hot.Stages) {
		stage := hot.Stages[name]
		fmt.Fprintf(w, "    %-14s %10s  n=%-6d %s/query\n",
			name, duration(stage.Total), stage.Count, duration(mean(stage.Total, stage.Count)))
	}
	phase := hot.Vector
	fmt.Fprintln(w, "  vector lane phases")
	fmt.Fprintf(w, "    load   %10s  hits=%d loads=%d  (warm %s, %d misses)\n",
		duration(phase.SnapshotLoad), phase.SnapshotHits, phase.SnapshotMisses,
		duration(phase.WarmLoad), phase.WarmMisses)
	fmt.Fprintf(w, "    embed  %10s  n=%d  mean %s\n",
		duration(phase.EmbedTotal), phase.Embeds, duration(hot.MeanEmbed))
	fmt.Fprintf(w, "    scan   %10s  = walk %s + cosine %s\n",
		duration(phase.ScanTotal), duration(phase.walk()), duration(phase.ScoreTotal))
	fmt.Fprintf(w, "           walk %.2fµs/entry over %.0f entries/query, cosine %.2fµs/entry over %.0f scored/query\n",
		perEntry(phase.walk(), phase.EntriesScanned), hot.ScannedPerQuery,
		perEntry(phase.ScoreTotal, phase.EntriesScored), hot.ScoredPerQuery)
	fmt.Fprintf(w, "    sort   %10s\n", duration(phase.SortTotal))
	fmt.Fprintf(w, "    scanned %d entries, scored %d (a filter may admit fewer than the table holds)\n",
		phase.EntriesScanned, phase.EntriesScored)
	fmt.Fprintln(w, "  shares")
	fmt.Fprintf(w, "    of the lane:   scan %.1f%%  sort %.1f%%  embed %.1f%%  load %.1f%%\n",
		hot.Shares.ScanOfLane*100, hot.Shares.SortOfLane*100, hot.Shares.EmbedOfLane*100, hot.Shares.LoadOfLane*100)
	fmt.Fprintf(w, "    scan of the search stage %.1f%%, of the summed request time %.1f%%\n\n",
		hot.Shares.ScanOfSearch*100, hot.Shares.ScanOfContext*100)
	fmt.Fprintln(w, "  projection (this run's per-entry slope, one scope's table)")
	fmt.Fprintf(w, "    %10s %10s %10s %12s\n", "entries", "scan", "sort", "scan/embed")
	for _, point := range hot.Projection {
		ratio := 0.0
		if hot.MeanEmbed > 0 {
			ratio = float64(point.Scan) / float64(hot.MeanEmbed)
		}
		fmt.Fprintf(w, "    %10d %10s %10s %11.1f%%\n",
			point.EntriesPerScope, duration(point.Scan), duration(point.Sort), ratio*100)
	}
	fmt.Fprintf(w, "    (scan is linear in entries, sort scales as n log n; embed is this run's mean %.1fms)\n\n",
		float64(hot.MeanEmbed)/float64(time.Millisecond))
}

func renderFootprint(w io.Writer, reading reading) {
	footprint := reading.Footprint
	fmt.Fprintln(w, "footprint (resident materialized tables, after the passes)")
	fmt.Fprintf(w, "  scopes %d  entries %d  dimensions %d  vector payload %s\n",
		footprint.Scopes, footprint.Entries, footprint.Dimensions, megabytes(footprint.VectorBytes))
	fmt.Fprintf(w, "  live heap %s → %s (delta %s)\n",
		megabytes(int64(reading.HeapBeforeBytes)), megabytes(int64(reading.HeapAfterBytes)),
		megabytes(deltaBytes(reading.HeapAfterBytes, reading.HeapBeforeBytes)))
	if reading.RSSAfterBytes > 0 {
		fmt.Fprintf(w, "  rss %s → %s\n",
			megabytes(int64(reading.RSSBeforeBytes)), megabytes(int64(reading.RSSAfterBytes)))
	}
	fmt.Fprintf(w, "  note: the payload counts vector bytes alone; entry addresses, metadata, and text sit on\n")
	fmt.Fprintf(w, "        top of it, so the heap delta is the reading to quote for a resident table\n")
}

func duration(value time.Duration) string {
	switch {
	case value == 0:
		return "0"
	case value < time.Millisecond:
		return fmt.Sprintf("%.0fµs", float64(value)/float64(time.Microsecond))
	case value < time.Second:
		return fmt.Sprintf("%.1fms", float64(value)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.2fs", float64(value)/float64(time.Second))
	}
}

func megabytes(bytes int64) string {
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1<<20))
}

func mean(total time.Duration, count int) time.Duration {
	if count <= 0 {
		return 0
	}
	return total / time.Duration(count)
}

// perEntry renders a total as microseconds per counted entry, the unit the
// scan costs are read in.
func perEntry(total time.Duration, count int64) float64 {
	if count <= 0 {
		return 0
	}
	return float64(total) / float64(count) / 1e3
}

func median(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func deltaBytes(after, before uint64) int64 {
	if after < before {
		return -int64(before - after)
	}
	return int64(after - before)
}
