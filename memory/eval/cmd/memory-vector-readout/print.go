package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// errorChain renders an error and the causes it does not itself spell out. A
// wrapper that formats its cause with %w already carries it, so only the levels
// that add something are appended: a memory error renders as its kind and
// capability alone, and what a stopped run needs to say -- which lane could not
// read what -- is what the levels beneath it hold.
func errorChain(err error) string {
	var text strings.Builder
	printed := ""
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := current.Error()
		if strings.Contains(printed, message) {
			continue
		}
		if printed != "" {
			text.WriteString(": ")
		}
		text.WriteString(message)
		printed = text.String()
	}
	return text.String()
}

func renderCold(w io.Writer, readings []coldReading) {
	if len(readings) == 0 {
		return
	}
	printf(w, "cold pass (%d scopes, the first question of each, sequential)\n", len(readings))
	printf(w, "  %-14s %10s %10s %6s %8s %10s %10s %6s %6s\n",
		"conversation", "latency", "load", "shards", "decodes", "resident+", "heap+d", "scans", "embeds")
	loads := make([]time.Duration, 0, len(readings))
	for _, reading := range readings {
		loads = append(loads, reading.ShardLoad)
		printf(w, "  %-14s %10s %10s %6d %8d %10s %10s %6d %6d\n",
			reading.ConversationID, duration(reading.Latency), duration(reading.ShardLoad),
			reading.ShardLoads, reading.Decodes, megabytes(reading.BytesAdded),
			megabytes(reading.HeapDeltaBytes), reading.Scans, reading.Embeds)
	}
	printf(w, "  median load %s\n", duration(median(loads)))
	printf(w, "  note: load is the question's own materialization of the shard it names, and decodes\n")
	printf(w, "        counts the questions that paid a decode rather than finding the table decoded;\n")
	printf(w, "        resident+ is the payload those tables added to what the store keeps.\n")
	printf(w, "        scans/embeds say whether the question was then served by the lane: a row with 0\n")
	printf(w, "        scans or 0 embeds read a table the lane did not answer with\n\n")
}

func renderHot(w io.Writer, hot *hotReading) {
	if hot == nil {
		return
	}
	printf(w, "hot pass (%d questions, %d workers, wall %s)\n", hot.Questions, hot.Workers, duration(hot.Wall))
	printf(w, "  latency  mean %s  p50 %s  p90 %s  p99 %s  max %s  over-5s %d\n",
		duration(hot.Latency.Mean), duration(hot.Latency.P50), duration(hot.Latency.P90),
		duration(hot.Latency.P99), duration(hot.Latency.Max), hot.Latency.Over5s)
	line(w, "  stages (sum over the pass, per-request mean)")
	for _, name := range sortedStages(hot.Stages) {
		stage := hot.Stages[name]
		printf(w, "    %-14s %10s  n=%-6d %s/query\n",
			name, duration(stage.Total), stage.Count, duration(mean(stage.Total, stage.Count)))
	}
	phase := hot.Vector
	line(w, "  vector lane phases")
	printf(w, "    load   %10s  tables=%d errors=%d\n",
		duration(phase.ShardLoad), phase.ShardLoads, phase.ShardErrors)
	printf(w, "           store: %d hits, %d decodes, %d evictions, %d uncached\n",
		phase.Hits, phase.Decodes, phase.Evictions, phase.Uncached)
	printf(w, "    embed  %10s  n=%d  mean %s\n",
		duration(phase.EmbedTotal), phase.Embeds, duration(hot.MeanEmbed))
	printf(w, "    scan   %10s  = walk %s + cosine %s\n",
		duration(phase.ScanTotal), duration(phase.walk()), duration(phase.ScoreTotal))
	printf(w, "           walk %.2fµs/entry over %.0f entries/query, cosine %.2fµs/entry over %.0f scored/query\n",
		perEntry(phase.walk(), phase.EntriesScanned), hot.ScannedPerQuery,
		perEntry(phase.ScoreTotal, phase.EntriesScored), hot.ScoredPerQuery)
	printf(w, "    sort   %10s\n", duration(phase.SortTotal))
	printf(w, "    scanned %d entries, scored %d (a filter may admit fewer than the table holds)\n",
		phase.EntriesScanned, phase.EntriesScored)
	if hot.Certification.Certified {
		line(w, "  self-certified: every question reached the lane (queries = scans = questions,")
		line(w, "    embeds = queries, no failed lookups)")
	} else {
		printf(w, "  NOT self-certified: %s\n", strings.Join(hot.Certification.Reasons, "; "))
		line(w, "    the numbers above describe a pass whose lane did not serve every question;")
		line(w, "    quotes belong to a run that certifies")
	}
	line(w, "  shares")
	printf(w, "    of the lane:   scan %.1f%%  sort %.1f%%  embed %.1f%%  load %.1f%%\n",
		hot.Shares.ScanOfLane*100, hot.Shares.SortOfLane*100, hot.Shares.EmbedOfLane*100, hot.Shares.LoadOfLane*100)
	printf(w, "    scan of the search stage %.1f%%, of the summed request time %.1f%%\n\n",
		hot.Shares.ScanOfSearch*100, hot.Shares.ScanOfContext*100)
	line(w, "  projection (this run's per-entry slope, one scope's table)")
	printf(w, "    %10s %10s %10s %12s\n", "entries", "scan", "sort", "scan/embed")
	for _, point := range hot.Projection {
		ratio := 0.0
		if hot.MeanEmbed > 0 {
			ratio = float64(point.Scan) / float64(hot.MeanEmbed)
		}
		printf(w, "    %10d %10s %10s %11.1f%%\n",
			point.EntriesPerShard, duration(point.Scan), duration(point.Sort), ratio*100)
	}
	printf(w, "    (scan is linear in entries, the ordering is the limit's work; embed is this run's mean %.1fms)\n\n",
		float64(hot.MeanEmbed)/float64(time.Millisecond))
}

func renderResident(w io.Writer, reading reading) {
	resident := reading.Resident
	line(w, "resident (tables the lane's store kept decoded, after the passes)")
	printf(w, "  tables %d  payload %s  hits %d  decodes %d  evictions %d  uncached %d\n",
		resident.Bases, megabytes(resident.Bytes), resident.Hits, resident.Misses,
		resident.Evictions, resident.Uncached)
	if resident.Evictions > 0 || resident.Uncached > 0 {
		// A resident state below what the shards read would hold is explained
		// here, and the configured budget is in the retrieval summary above.
		printf(w, "  budget: %d tables evicted, %d reads of a table too large to keep\n",
			resident.Evictions, resident.Uncached)
	}
	printf(w, "  live heap %s → %s (delta %s)\n",
		megabytes(int64(reading.HeapBeforeBytes)), megabytes(int64(reading.HeapAfterBytes)),
		megabytes(deltaBytes(reading.HeapAfterBytes, reading.HeapBeforeBytes)))
	if reading.RSSAfterBytes > 0 {
		printf(w, "  rss %s → %s\n",
			megabytes(int64(reading.RSSBeforeBytes)), megabytes(int64(reading.RSSAfterBytes)))
	}
	printf(w, "  note: the payload counts the tables' vector bytes alone, the term the budget weighs;\n")
	printf(w, "        entry addresses, metadata, and text sit on top of it, so the heap delta is the\n")
	printf(w, "        reading to quote for a resident table\n")
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

// printf and line render one line to a reader. The targets are stdout and test
// buffers, where a write error is not recoverable and says nothing about the
// reading already taken, so the error is deliberately dropped here.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func line(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
