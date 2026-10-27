package vector

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// cacheScope is the one scope the fixtures below live in. The lane's tables are
// bounded per shard, so the fixtures are conversations of one scope: a
// derivation fans out across conversations, and that is the shape the bounds
// are for.
func cacheScope() corememory.Scope {
	return corememory.Scope{RuntimeID: "runtime", UserID: "user"}
}

// warmTotal is every load a write's warm-up ran: a hit, a load, or a failure.
func warmTotal(index *Index) int64 {
	stats := index.Stats()
	return stats.WarmHits + stats.WarmMisses + stats.WarmErrors
}

// buildCacheFixture rebuilds the scope holding one artifact per conversation
// and waits for the warm that follows each shard's write to finish. Waiting
// keeps the reads that follow deterministic: recency is only moved by reads the
// test itself makes.
func buildCacheFixture(t *testing.T, index *Index, scope corememory.Scope, conversations ...string) {
	t.Helper()
	artifacts := make([]component.Artifact, 0, len(conversations))
	for _, conversation := range conversations {
		artifacts = append(artifacts, conversationArtifact(conversation, "a-"+conversation, "alpha"))
	}
	before := warmTotal(index)
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: artifacts,
	}); err != nil {
		t.Fatal(err)
	}
	want := before + int64(len(conversations))
	waitForStats(t, index, "every rebuild warming its shard", func(Stats) bool {
		return warmTotal(index) == want
	})
}

// readShard serves one query against one conversation's shard and returns what
// it answered, together with what that read moved: one hit when the table was
// resident, one miss when the read materialized it. Reading deltas rather than
// totals keeps every step's expectation about that step.
func readShard(
	t *testing.T,
	index *Index,
	scope corememory.Scope,
	conversation string,
) ([]component.Candidate, int64, int64) {
	t.Helper()
	before := index.Stats()
	results, err := index.SearchVector(context.Background(), component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
		Filter: component.VectorSearchFilter{
			Metadata: corememory.Metadata{"conversation_id": conversation},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	after := index.Stats()
	return results, after.SnapshotHits - before.SnapshotHits, after.SnapshotMisses - before.SnapshotMisses
}

// TestCacheBoundEvictsTheLeastRecentlyReadShard pins the shard bound: the
// resident set stays at the bound, and what leaves it is the table the lane
// read least recently -- not the one a map happened to hand over.
//
// The warms that follow the rebuild are concurrent, so which of the three
// tables survives them is not fixed; the test reads the shards in a known order
// first, which is what fixes the recency the evictions then follow, and checks
// every read on the counters that read moved.
func TestCacheBoundEvictsTheLeastRecentlyReadShard(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{
		KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts",
		Limits: CacheLimits{Shards: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := cacheScope()
	first, second, third := "first", "second", "third"
	buildCacheFixture(t, index, scope, first, second, third)
	// Three tables were built into a cache that holds two.
	if stats := index.Stats(); stats.Footprint.Shards != 2 || stats.Evictions != 1 {
		t.Fatalf("after three rebuilds: shards %d, evictions %d", stats.Footprint.Shards, stats.Evictions)
	}
	for _, conversation := range []string{first, second, third} {
		readShard(t, index, scope, conversation)
	}
	// The reads above leave the bound holding the two shards read last, in the
	// order they were read. From here every read's outcome is fixed.
	if stats := index.Stats(); stats.Footprint.Shards != 2 {
		t.Fatalf("the bound holds %d shards", stats.Footprint.Shards)
	}
	before := index.Stats()
	steps := []struct {
		why          string
		conversation string
		hits         int64
		misses       int64
	}{
		{why: "the table read most recently", conversation: third, hits: 1},
		{why: "the other table the bound kept", conversation: second, hits: 1},
		{why: "the table read least recently, which the bound does not hold", conversation: first, misses: 1},
		{why: "the table that read did not evict", conversation: second, hits: 1},
		{why: "the table that read evicted, being the least recently read", conversation: third, misses: 1},
	}
	for _, step := range steps {
		results, hits, misses := readShard(t, index, scope, step.conversation)
		if len(results) != 1 {
			t.Fatalf("%s answered %+v", step.why, results)
		}
		if hits != step.hits || misses != step.misses {
			t.Fatalf("%s: hits %d, misses %d -- want hits %d, misses %d",
				step.why, hits, misses, step.hits, step.misses)
		}
	}
	stats := index.Stats()
	if stats.Footprint.Shards != 2 {
		t.Fatalf("the bound does not hold after the reads: %+v", stats.Footprint)
	}
	// Two of those reads materialized a table the bound had dropped, and each
	// of them evicted the table read least recently at that point.
	if evictions := stats.Evictions - before.Evictions; evictions != 2 {
		t.Fatalf("the reads evicted %d tables, want 2", evictions)
	}
}

// TestCacheEntryBoundServesWithoutKeepingTheTable pins the per-shard bound: a
// table larger than the bound is not kept, every read of that shard pays for
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
	scope := cacheScope()
	artifacts := []component.Artifact{
		conversationArtifact("conversation", "a", "alpha"),
		conversationArtifact("conversation", "b", "alpha again"),
		conversationArtifact("conversation", "c", "beta"),
	}
	if err := bounded.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: artifacts,
	}); err != nil {
		t.Fatal(err)
	}
	waitForStats(t, bounded, "the rebuild warming its shard", func(stats Stats) bool {
		return stats.UncachedLoads == 1
	})
	if footprint := bounded.Stats().Footprint; footprint != (Footprint{}) {
		t.Fatalf("a table past the entry bound was kept: %+v", footprint)
	}
	got, hits, misses := readShard(t, bounded, scope, "conversation")
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
	want, _, _ := readShard(t, held, scope, "conversation")
	if len(want) != len(got) {
		t.Fatalf("answers differ: %d candidates against %d", len(want), len(got))
	}
	for position := range want {
		if !sameCandidate(want[position], got[position]) {
			t.Fatalf("candidate %d = %+v, want %+v", position, got[position], want[position])
		}
	}
	if footprint := held.Stats().Footprint; footprint.Shards != 1 || footprint.Entries != 3 {
		t.Fatalf("a lane with no bound kept %+v", footprint)
	}
}

// TestCacheBoundsDoNotTouchTheAnswer pins both bounds against the reading they
// are tempting to take: a bounded cache changes the footprint and the number of
// times a table is read, and nothing about what a query returns.
func TestCacheBoundsDoNotTouchTheAnswer(t *testing.T) {
	runtime, model := fakeRuntime(t)
	scope := cacheScope()
	conversations := []string{"one", "two", "three"}
	artifacts := make([]component.Artifact, 0, 3*len(conversations))
	for _, conversation := range conversations {
		artifacts = append(artifacts,
			conversationArtifact(conversation, "a-"+conversation, "alpha"),
			conversationArtifact(conversation, "b-"+conversation, "beta"),
			conversationArtifact(conversation, "c-"+conversation, "alpha again"),
		)
	}
	var answers [][]component.Candidate
	for _, limits := range []CacheLimits{{}, {Shards: 1}, {Entries: 2}, {Shards: 1, Entries: 2}} {
		index, err := New(Config{
			KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts", Limits: limits,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := index.Rebuild(context.Background(), component.ProjectionRequest{
			Scope: scope, Artifacts: artifacts,
		}); err != nil {
			t.Fatal(err)
		}
		var run []component.Candidate
		for _, conversation := range conversations {
			results, _, _ := readShard(t, index, scope, conversation)
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
