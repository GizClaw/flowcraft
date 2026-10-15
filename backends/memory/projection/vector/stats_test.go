package vector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// unreadableStore fails every read, which is what the lane meets when the
// state it is bound to read cannot be served. It stays a workspace store in
// every other respect, so the lane sees the substrate it would serve from.
type unreadableStore struct{ *storage.WorkspaceKV }

func (store unreadableStore) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("storage: state cannot be read")
}

// TestStatsCountsAFailedLookupApartFromHitsAndLoads pins the counter split: a
// lookup that returned an error used to be recorded as a hit, so a reading of
// a lane that served nothing showed a resident table it never read. A hit, a
// load, and a failed lookup are three outcomes and are counted apart.
func TestStatsCountsAFailedLookupApartFromHitsAndLoads(t *testing.T) {
	runtime, model := fakeRuntime(t)
	kvStore, ok := kvFor(t).(*storage.WorkspaceKV)
	if !ok {
		t.Fatalf("kvFor returned %T", kvFor(t))
	}
	index, err := New(Config{KV: unreadableStore{kvStore}, Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if _, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "alpha"}); err == nil {
		t.Fatal("a lookup that cannot read its state returned a result")
	}
	stats := index.Stats()
	if stats.SnapshotErrors != 1 || stats.SnapshotHits != 0 || stats.SnapshotMisses != 0 {
		t.Fatalf("failed lookup counted as %+v", stats)
	}
}

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
	// The assertions below are counts and invariants, not nonzero durations:
	// the fakes work in nanoseconds, and a platform whose clock cannot resolve
	// that would read a recorded phase as zero. The counts pin that every phase
	// was recorded; the clock is pinned by
	// TestStatsMeasuresWhatTheEmbedTook, whose embed is slow enough to read.
	if stats.Queries != 2 {
		t.Fatalf("queries = %d", stats.Queries)
	}
	if stats.SnapshotHits != 2 || stats.SnapshotMisses != 0 {
		t.Fatalf("serving-path snapshot hits/misses = %d/%d: a write's warm is not a query's load",
			stats.SnapshotHits, stats.SnapshotMisses)
	}
	if stats.WarmHits != 0 || stats.WarmMisses != 1 {
		t.Fatalf("warm hits/misses = %d/%d", stats.WarmHits, stats.WarmMisses)
	}
	if stats.Embeds != 1 {
		t.Fatalf("embeds = %d: only the text search may embed a query", stats.Embeds)
	}
	if stats.Scans != 2 || stats.EntriesScanned != 6 || stats.EntriesScored != 6 {
		t.Fatalf("scans/scanned/scored = %d/%d/%d", stats.Scans, stats.EntriesScanned, stats.EntriesScored)
	}
	if stats.ScoreTotal > stats.ScanTotal {
		t.Fatalf("score total = %v exceeds the scan total %v: the walk cannot be negative",
			stats.ScoreTotal, stats.ScanTotal)
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
	index.invalidateScope(scope)
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
	index.invalidateScope(scope)
	if _, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "alpha"}); err != nil {
		t.Fatal(err)
	}
	stats := index.Stats()
	if stats.SnapshotMisses != 1 || stats.SnapshotHits != 0 {
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

// TestStatsMeasuresWhatTheEmbedTook pins the attribution itself, on every
// clock: the fake embed sleeps, so the phase total has to be at least that.
func TestStatsMeasuresWhatTheEmbedTook(t *testing.T) {
	const embedTime = 2 * time.Millisecond
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		time.Sleep(embedTime)
		vectors := make([][]float32, len(texts))
		for index := range vectors {
			vectors[index] = []float32{1, 0}
		}
		return vectors
	})
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
	if _, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "alpha"}); err != nil {
		t.Fatal(err)
	}
	stats := index.Stats()
	if stats.Embeds != 1 || stats.EmbedTotal < embedTime {
		t.Fatalf("embeds/total = %d/%v: the lane has to measure the embed it served",
			stats.Embeds, stats.EmbedTotal)
	}
}
