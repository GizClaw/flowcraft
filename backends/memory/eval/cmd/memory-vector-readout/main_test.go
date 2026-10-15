package main

import (
	"testing"
	"time"

	flowmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/projection/vector"
)

func TestDistributionNearestRankAndOver5s(t *testing.T) {
	values := make([]time.Duration, 0, 100)
	for index := 1; index <= 100; index++ {
		values = append(values, time.Duration(index)*time.Millisecond)
	}
	values[99] = 6 * time.Second
	reading := distribution(values)
	if reading.P50 != 50*time.Millisecond || reading.P90 != 90*time.Millisecond {
		t.Fatalf("p50/p90 = %v/%v", reading.P50, reading.P90)
	}
	if reading.Max != 6*time.Second || reading.Over5s != 1 {
		t.Fatalf("max/over-5s = %v/%d", reading.Max, reading.Over5s)
	}
	if reading.Mean != 109500*time.Microsecond {
		t.Fatalf("mean = %v, want the 1..100ms series' mean with 6s in place of 100ms", reading.Mean)
	}
}

func TestPhaseDeltaAndShares(t *testing.T) {
	before := vector.Stats{
		Queries: 3, SnapshotHits: 3, ScanTotal: 30 * time.Millisecond,
		Embeds: 3, EmbedTotal: 300 * time.Millisecond, SortTotal: 3 * time.Millisecond,
		EntriesScanned: 300, EntriesScored: 300, Scans: 3,
	}
	after := vector.Stats{
		Queries: 5, SnapshotHits: 4, SnapshotMisses: 1, SnapshotLoad: 8 * time.Millisecond,
		Scans: 5, ScanTotal: 50 * time.Millisecond, SortTotal: 5 * time.Millisecond,
		Embeds: 5, EmbedTotal: 500 * time.Millisecond,
		EntriesScanned: 500, EntriesScored: 500,
	}
	phase := phaseDelta(before, after)
	if phase.Queries != 2 || phase.Scans != 2 || phase.EntriesScanned != 200 {
		t.Fatalf("phase = %+v", phase)
	}
	if phase.ScanTotal != 20*time.Millisecond || phase.SnapshotLoad != 8*time.Millisecond {
		t.Fatalf("phase = %+v", phase)
	}
	stages := map[string]stageReading{
		"lane:vector": {Total: 500 * time.Millisecond, Count: 2},
		"search":      {Total: 800 * time.Millisecond, Count: 2},
	}
	shares := computeShares(phase, stages, 2*time.Second)
	if shares.ScanOfLane != 0.04 || shares.ScanOfSearch != 0.025 || shares.ScanOfContext != 0.01 {
		t.Fatalf("shares = %+v", shares)
	}
	if shares.EmbedOfLane != 0.4 {
		t.Fatalf("embed share = %v", shares.EmbedOfLane)
	}
}

func TestStageDeltasKeepPerStageCounts(t *testing.T) {
	before := map[string]flowmemory.StageStat{
		"lane:vector": {Total: time.Second, Count: 4},
		"pack":        {Total: 100 * time.Millisecond, Count: 4},
	}
	after := map[string]flowmemory.StageStat{
		"lane:vector": {Total: 3 * time.Second, Count: 10},
		"pack":        {Total: 150 * time.Millisecond, Count: 10},
	}
	deltas := stageDeltas(before, after)
	if deltas["lane:vector"].Total != 2*time.Second || deltas["lane:vector"].Count != 6 {
		t.Fatalf("lane delta = %+v", deltas["lane:vector"])
	}
	if deltas["pack"].Total != 50*time.Millisecond || deltas["pack"].Count != 6 {
		t.Fatalf("pack delta = %+v", deltas["pack"])
	}
}

func TestProjectionsScaleScanLinearlyAndSortAsNLogN(t *testing.T) {
	phase := vectorPhase{Scans: 2, ScanTotal: 20 * time.Millisecond, SortTotal: time.Millisecond, EntriesScanned: 2000}
	points := projections(phase, 1000, 2048)
	if len(points) != 4 {
		t.Fatalf("points = %+v", points)
	}
	if points[0].EntriesPerScope != 1000 || points[0].Scan != 10*time.Millisecond {
		t.Fatalf("current point = %+v", points[0])
	}
	// Ten times the entries cost ten times the scan, and the sort grows a
	// little faster than ten times.
	tenX := points[1]
	if tenX.EntriesPerScope != 10000 || tenX.Scan != 100*time.Millisecond {
		t.Fatalf("10k point = %+v", tenX)
	}
	if tenX.Sort <= 10*points[0].Sort {
		t.Fatalf("sort did not grow with n log n: %v vs %v", tenX.Sort, points[0].Sort)
	}
}

func TestDurationAndMegabytesRenderUnits(t *testing.T) {
	if got := duration(1500 * time.Microsecond); got != "1.5ms" {
		t.Fatalf("duration = %q", got)
	}
	if got := duration(2 * time.Second); got != "2.00s" {
		t.Fatalf("duration = %q", got)
	}
	if got := megabytes(3 << 20); got != "3.0MB" {
		t.Fatalf("megabytes = %q", got)
	}
}
