package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	flowmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/projection/vector"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestErrorChainNamesEveryCause pins what a stopped run says: the memory error
// renders as its kind and capability alone, so the renderer walks the chain to
// reach the report that names the lane and what it could not read.
func TestErrorChainNamesEveryCause(t *testing.T) {
	err := corememory.NewError(corememory.KindProviderFailure, "context",
		fmt.Errorf("retrieval: lane %q cannot read its state: %w", "vector",
			fmt.Errorf("memory component: %s: state cannot be read: %w", "facts.vector-v1",
				errors.New("projection: segment 0 digest mismatch"))))
	text := errorChain(err)
	for _, want := range []string{
		"memory: provider_failure capability=context",
		`lane "vector" cannot read its state`,
		"facts.vector-v1: state cannot be read",
		"projection: segment 0 digest mismatch",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("chain %q does not name %q", text, want)
		}
	}
	// A wrapper already carries its cause, and the chain walks the same report
	// twice, so the renderer states each level once.
	if count := strings.Count(text, "cannot read its state"); count != 1 {
		t.Fatalf("chain %q states the lane %d times, want once", text, count)
	}
}

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

// TestCertificationStatesWhetherTheLaneServedThePass pins what a reading may
// be quoted for: the counts the lane recorded have to agree with the questions
// the pass asked. A pass where any question did not reach the lane is reported
// as not certified, with the disagreement named -- the numbers of such a pass
// describe the other lanes, not the vector lane.
func TestCertificationStatesWhetherTheLaneServedThePass(t *testing.T) {
	complete := vectorPhase{Queries: 4, Scans: 4, Embeds: 4}
	if got := certify(complete, 4); !got.Certified || len(got.Reasons) != 0 {
		t.Fatalf("complete pass = %+v", got)
	}
	for name, phase := range map[string]vectorPhase{
		"a question the lane never saw":    {Queries: 3, Scans: 4, Embeds: 3},
		"a question that was not embedded": {Queries: 4, Scans: 4, Embeds: 3},
		"a lookup that failed":             {Queries: 4, Scans: 4, Embeds: 4, SnapshotErrors: 1},
	} {
		t.Run(name, func(t *testing.T) {
			got := certify(phase, 4)
			if got.Certified || len(got.Reasons) == 0 {
				t.Fatalf("pass = %+v, want a stated reason", got)
			}
		})
	}
}

func TestPhaseDeltaAndShares(t *testing.T) {
	before := vector.Stats{
		Queries: 3, SnapshotHits: 3, ScanTotal: 30 * time.Millisecond,
		Embeds: 3, EmbedTotal: 300 * time.Millisecond, SortTotal: 3 * time.Millisecond,
		EntriesScanned: 300, EntriesScored: 300, Scans: 3,
		WarmHits: 1, WarmMisses: 1, WarmLoad: 4 * time.Second,
	}
	after := vector.Stats{
		Queries: 5, SnapshotHits: 4, SnapshotMisses: 1, SnapshotLoad: 8 * time.Millisecond,
		SnapshotErrors: 2,
		Scans:          5, ScanTotal: 50 * time.Millisecond, SortTotal: 5 * time.Millisecond,
		Embeds: 5, EmbedTotal: 500 * time.Millisecond,
		EntriesScanned: 500, EntriesScored: 500,
		WarmHits: 3, WarmMisses: 1, WarmErrors: 1, WarmLoad: 9 * time.Second,
	}
	phase := phaseDelta(before, after)
	if phase.Queries != 2 || phase.Scans != 2 || phase.EntriesScanned != 200 {
		t.Fatalf("phase = %+v", phase)
	}
	if phase.ScanTotal != 20*time.Millisecond || phase.SnapshotLoad != 8*time.Millisecond {
		t.Fatalf("phase = %+v", phase)
	}
	// The warm a deployment pays beside the queries is reported apart, so a
	// reader never adds it to the load the queries paid.
	if phase.WarmLoad != 5*time.Second || phase.WarmHits != 2 || phase.WarmMisses != 0 {
		t.Fatalf("warm phase = %+v, want the warm's own delta", phase)
	}
	// Lookups that failed are their own bucket, and the phase carries them:
	// they are what tells a pass that lost the lane apart from one that
	// served it.
	if phase.SnapshotErrors != 2 || phase.WarmErrors != 1 {
		t.Fatalf("error phase = %+v", phase)
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

// TestProjectionsScaleTheScanAndKeepTheOrdering pins the model the table is
// read with: the scan is linear in the table -- the score and the selection
// offer happen once per admitted entry -- while the ordering is the work of
// the limit the scan asked for, so a bigger table does not order more.
func TestProjectionsScaleTheScanAndKeepTheOrdering(t *testing.T) {
	phase := vectorPhase{Scans: 2, ScanTotal: 20 * time.Millisecond, SortTotal: time.Millisecond, EntriesScanned: 2000}
	points := projections(phase, 1000, 2048)
	if len(points) != 4 {
		t.Fatalf("points = %+v", points)
	}
	if points[0].EntriesPerScope != 1000 || points[0].Scan != 10*time.Millisecond {
		t.Fatalf("current point = %+v", points[0])
	}
	// Ten times the entries cost ten times the scan, and the ordering stays at
	// what one scan of this run cost.
	tenX := points[1]
	if tenX.EntriesPerScope != 10000 || tenX.Scan != 100*time.Millisecond {
		t.Fatalf("10k point = %+v", tenX)
	}
	if tenX.Sort != points[0].Sort || points[0].Sort != 500*time.Microsecond {
		t.Fatalf("ordering = %v at 10k and %v at 1k", tenX.Sort, points[0].Sort)
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
