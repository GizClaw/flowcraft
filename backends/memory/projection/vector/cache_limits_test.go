package vector

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// cacheScope is one scope of the fixtures below.
func cacheScope(name string) corememory.Scope {
	return corememory.Scope{RuntimeID: "runtime", UserID: name}
}

// buildCacheFixture rebuilds one scope holding one artifact and waits for the
// warm that follows the write to finish. Waiting keeps the reads that follow
// deterministic: recency is only moved by reads the test itself makes.
func buildCacheFixture(t *testing.T, index *Index, scopes ...corememory.Scope) {
	t.Helper()
	before := index.Stats().WarmHits + index.Stats().WarmMisses + index.Stats().WarmErrors
	for _, scope := range scopes {
		if err := index.Rebuild(context.Background(), component.ProjectionRequest{
			Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	want := before + int64(len(scopes))
	waitForStats(t, index, "every rebuild warming its scope", func(stats Stats) bool {
		return stats.WarmHits+stats.WarmMisses+stats.WarmErrors == want
	})
}

// readScope serves one query and returns what it answered, together with what
// that read moved: one hit when the table was resident, one miss when the read
// materialized it. Reading deltas rather than totals keeps every step's
// expectation about that step.
func readScope(t *testing.T, index *Index, scope corememory.Scope) ([]component.Candidate, int64, int64) {
	t.Helper()
	before := index.Stats()
	results, err := index.SearchVector(context.Background(), component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	after := index.Stats()
	return results, after.SnapshotHits - before.SnapshotHits, after.SnapshotMisses - before.SnapshotMisses
}

// TestCacheBoundEvictsTheLeastRecentlyReadScope pins the scope bound: the
// resident set stays at the bound, and what leaves it is the table the lane
// read least recently -- not the one a map happened to hand over.
//
// The warms that follow the three writes are concurrent, so which of the three
// tables survives them is not fixed; the test reads the scopes in a known order
// first, which is what fixes the recency the evictions then follow, and checks
// every read on the counters that read moved.
func TestCacheBoundEvictsTheLeastRecentlyReadScope(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{
		KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts",
		Limits: CacheLimits{Scopes: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, second, third := cacheScope("first"), cacheScope("second"), cacheScope("third")
	buildCacheFixture(t, index, first, second, third)
	// Three tables were built into a cache that holds two.
	if stats := index.Stats(); stats.Footprint.Scopes != 2 || stats.Evictions != 1 {
		t.Fatalf("after three rebuilds: scopes %d, evictions %d", stats.Footprint.Scopes, stats.Evictions)
	}
	for _, scope := range []corememory.Scope{first, second, third} {
		readScope(t, index, scope)
	}
	// The reads above leave the bound holding the two scopes read last, in the
	// order they were read. From here every read's outcome is fixed.
	if stats := index.Stats(); stats.Footprint.Scopes != 2 {
		t.Fatalf("the bound holds %d scopes", stats.Footprint.Scopes)
	}
	before := index.Stats()
	steps := []struct {
		why    string
		scope  corememory.Scope
		hits   int64
		misses int64
	}{
		{why: "the table read most recently", scope: third, hits: 1},
		{why: "the other table the bound kept", scope: second, hits: 1},
		{why: "the table read least recently, which the bound does not hold", scope: first, misses: 1},
		{why: "the table that read did not evict", scope: second, hits: 1},
		{why: "the table that read evicted, being the least recently read", scope: third, misses: 1},
	}
	for _, step := range steps {
		results, hits, misses := readScope(t, index, step.scope)
		if len(results) != 1 {
			t.Fatalf("%s answered %+v", step.why, results)
		}
		if hits != step.hits || misses != step.misses {
			t.Fatalf("%s: hits %d, misses %d -- want hits %d, misses %d",
				step.why, hits, misses, step.hits, step.misses)
		}
	}
	stats := index.Stats()
	if stats.Footprint.Scopes != 2 {
		t.Fatalf("the bound does not hold after the reads: %+v", stats.Footprint)
	}
	// Two of those reads materialized a table the bound had dropped, and each
	// of them evicted the table read least recently at that point.
	if evictions := stats.Evictions - before.Evictions; evictions != 2 {
		t.Fatalf("the reads evicted %d tables, want 2", evictions)
	}
}

// TestCacheEntryBoundServesWithoutKeepingTheTable pins the per-scope bound: a
// table larger than the bound is not kept, every read of that scope pays for
// it, and the query is answered exactly as it would be with the table held.
func TestCacheEntryBoundServesWithoutKeepingTheTable(t *testing.T) {
	runtime, model := fakeRuntime(t)
	bounded, err := New(Config{
		KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts",
		Limits: CacheLimits{Entries: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := cacheScope("scope")
	artifacts := []component.Artifact{
		vectorArtifact("a", "alpha"), vectorArtifact("b", "alpha again"), vectorArtifact("c", "beta"),
	}
	if err := bounded.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: artifacts,
	}); err != nil {
		t.Fatal(err)
	}
	waitForStats(t, bounded, "the rebuild warming its scope", func(stats Stats) bool {
		return stats.UncachedLoads == 1
	})
	if footprint := bounded.Stats().Footprint; footprint != (Footprint{}) {
		t.Fatalf("a table past the entry bound was kept: %+v", footprint)
	}
	got, hits, misses := readScope(t, bounded, scope)
	if len(got) != 3 {
		t.Fatalf("the unbounded answer has three entries, this one has %d", len(got))
	}
	if hits != 0 || misses != 1 {
		t.Fatalf("the read of a table the cache refuses: hits %d, misses %d", hits, misses)
	}
	stats := bounded.Stats()
	if stats.UncachedLoads != 2 {
		t.Fatalf("the read was not counted as a load of a table it did not keep: %+v", stats)
	}
	if stats.Footprint != (Footprint{}) {
		t.Fatalf("the cache kept the table anyway: %+v", stats.Footprint)
	}

	// The same query on a lane with no bound: the answers have to agree, and
	// only the residency may differ.
	held, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: artifacts,
	}); err != nil {
		t.Fatal(err)
	}
	want, _, _ := readScope(t, held, scope)
	if len(want) != len(got) {
		t.Fatalf("answers differ: %d candidates against %d", len(want), len(got))
	}
	for position := range want {
		if !sameCandidate(want[position], got[position]) {
			t.Fatalf("candidate %d = %+v, want %+v", position, got[position], want[position])
		}
	}
	if footprint := held.Stats().Footprint; footprint.Entries != 3 {
		t.Fatalf("a lane with no bound kept %+v", footprint)
	}
}

// TestCacheBoundsDoNotTouchTheAnswer pins both bounds against the reading they
// are tempting to take: a bounded cache changes the footprint and the number of
// times a table is read, and nothing about what a query returns.
func TestCacheBoundsDoNotTouchTheAnswer(t *testing.T) {
	runtime, model := fakeRuntime(t)
	scopes := []corememory.Scope{cacheScope("one"), cacheScope("two"), cacheScope("three")}
	artifacts := []component.Artifact{
		vectorArtifact("a", "alpha"), vectorArtifact("b", "beta"), vectorArtifact("c", "alpha again"),
	}
	var answers [][]component.Candidate
	for _, limits := range []CacheLimits{{}, {Scopes: 1}, {Entries: 2}, {Scopes: 1, Entries: 2}} {
		index, err := New(Config{
			KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts", Limits: limits,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range scopes {
			if err := index.Rebuild(context.Background(), component.ProjectionRequest{
				Scope: scope, Artifacts: artifacts,
			}); err != nil {
				t.Fatal(err)
			}
		}
		var run []component.Candidate
		for _, scope := range scopes {
			results, err := index.SearchVector(context.Background(), component.VectorSearchRequest{
				Scope: scope, Vector: []float32{0.5, 0.5},
			})
			if err != nil {
				t.Fatal(err)
			}
			run = append(run, results...)
		}
		answers = append(answers, run)
	}
	for index := 1; index < len(answers); index++ {
		if len(answers[index]) != len(answers[0]) {
			t.Fatalf("bounds %d answered %d candidates, want %d", index, len(answers[index]), len(answers[0]))
		}
		for position := range answers[0] {
			if !sameCandidate(answers[index][position], answers[0][position]) {
				t.Fatalf("bounds %d: candidate %d = %+v, want %+v",
					index, position, answers[index][position], answers[0][position])
			}
		}
	}
}
