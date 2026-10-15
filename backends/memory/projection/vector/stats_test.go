package vector

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// waitForStats polls until the reading satisfies want, so a test never races
// the background warm a write starts.
func waitForStats(t *testing.T, index *Index, why string, want func(Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if want(index.Stats()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stats never reached %s: %+v", why, index.Stats())
}

func TestStatsAttributesPhasesAndFootprint(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	artifacts := []component.Artifact{
		vectorArtifact("a", "alpha"), vectorArtifact("b", "beta"), vectorArtifact("c", "alpha again"),
	}
	ctx := context.Background()
	if err := index.Rebuild(ctx, component.ProjectionRequest{Scope: scope, Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	// The warm a write starts materializes the table; it is counted apart, so
	// the two reads below are the serving path's hits.
	waitForStats(t, index, "the warm load", func(stats Stats) bool { return stats.WarmMisses == 1 })
	if _, err := index.Search(ctx, component.SearchRequest{Scope: scope, Query: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	stats := index.Stats()
	if stats.Queries != 2 {
		t.Fatalf("queries = %d", stats.Queries)
	}
	if stats.SnapshotHits != 2 || stats.SnapshotMisses != 0 {
		t.Fatalf("serving-path snapshot hits/misses = %d/%d: a write's warm is not a query's load",
			stats.SnapshotHits, stats.SnapshotMisses)
	}
	if stats.WarmHits != 0 || stats.WarmMisses != 1 || stats.WarmLoad <= 0 {
		t.Fatalf("warm hits/misses/load = %d/%d/%v", stats.WarmHits, stats.WarmMisses, stats.WarmLoad)
	}
	if stats.Embeds != 1 {
		t.Fatalf("embeds = %d: only the text search may embed a query", stats.Embeds)
	}
	if stats.EmbedTotal <= 0 {
		t.Fatalf("embed total = %v", stats.EmbedTotal)
	}
	if stats.Scans != 2 || stats.EntriesScanned != 6 || stats.EntriesScored != 6 {
		t.Fatalf("scans/scanned/scored = %d/%d/%d", stats.Scans, stats.EntriesScanned, stats.EntriesScored)
	}
	if stats.ScanTotal <= 0 || stats.SortTotal < 0 {
		t.Fatalf("scan/sort totals = %v/%v", stats.ScanTotal, stats.SortTotal)
	}
	if stats.ScoreTotal <= 0 || stats.ScoreTotal > stats.ScanTotal {
		t.Fatalf("score total = %v, outside (0, scan total %v]", stats.ScoreTotal, stats.ScanTotal)
	}
	if stats.Footprint.Scopes != 1 || stats.Footprint.Entries != 3 ||
		stats.Footprint.Dimensions != 2 || stats.Footprint.VectorBytes != 3*2*4 {
		t.Fatalf("footprint = %+v", stats.Footprint)
	}
}

func TestStatsScoredFollowsTheFilter(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha"), vectorArtifact("b", "beta")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := index.SearchVector(context.Background(), component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
		Filter: component.VectorSearchFilter{Name: "summary"},
	}); err != nil {
		t.Fatal(err)
	}
	stats := index.Stats()
	if stats.EntriesScanned != 2 || stats.EntriesScored != 0 {
		t.Fatalf("scanned/scored = %d/%d: a filtered-out entry is scanned, not scored",
			stats.EntriesScanned, stats.EntriesScored)
	}
}

func TestFootprintFollowsCacheInvalidation(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
	}); err != nil {
		t.Fatal(err)
	}
	waitForStats(t, index, "the warm load", func(stats Stats) bool { return stats.Footprint.Entries == 1 })
	index.invalidateSnapshots()
	if footprint := index.Stats().Footprint; footprint != (Footprint{}) {
		t.Fatalf("footprint after invalidation = %+v", footprint)
	}
}

// TestStatsKeepsAQueryThatMaterializesOnTheServingPath pins the split from the
// other side: dropping the cache without warming it leaves the next query to
// materialize the table, and that read is the query's own.
func TestStatsKeepsAQueryThatMaterializesOnTheServingPath(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
	}); err != nil {
		t.Fatal(err)
	}
	waitForStats(t, index, "the warm load", func(stats Stats) bool { return stats.WarmMisses == 1 })
	index.invalidateSnapshots()
	if _, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "alpha"}); err != nil {
		t.Fatal(err)
	}
	stats := index.Stats()
	if stats.SnapshotMisses != 1 || stats.SnapshotLoad <= 0 {
		t.Fatalf("the query that materialized the table was not counted on the serving path: %+v", stats)
	}
	if stats.WarmMisses != 1 {
		t.Fatalf("warm misses = %d: the query's miss must not land in the warm counters", stats.WarmMisses)
	}
	if _, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if after := index.Stats(); after.SnapshotHits != 1 || after.SnapshotMisses != 1 {
		t.Fatalf("second read = hits %d misses %d, want one hit", after.SnapshotHits, after.SnapshotMisses)
	}
}
