package vector

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestWriteInvalidatesOnlyTheShardItWrote pins what a write drops. One scope
// holds two conversations, each with a resident table; a delta lands in one of
// them. The written shard's table has to be dropped -- the warm that follows
// reloads it, which is how this test sees the drop -- and the other shard's
// table has to survive: its next query is served from the cache, not
// re-materialized.
//
// Before the lane invalidated per shard, any write dropped every table of the
// scope, so a host deriving one conversation paid the other conversations'
// materialization again on their next query (9.6s locally for the eval table,
// per shard).
func TestWriteInvalidatesOnlyTheShardItWrote(t *testing.T) {
	ctx := context.Background()
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	written, untouched := "written", "untouched"
	if err := index.Rebuild(ctx, component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{
			conversationArtifact(written, "a", "alpha"),
			conversationArtifact(untouched, "a", "alpha"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Both shards are warmed by the rebuild: two tables, two background loads.
	waitForStats(t, index, "both rebuilds warming their shard", func(stats Stats) bool {
		return stats.WarmMisses == 2
	})
	if footprint := index.Stats().Footprint; footprint.Shards != 2 || footprint.Entries != 2 {
		t.Fatalf("footprint after the rebuild = %+v", footprint)
	}
	if _, err := index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
		Filter: component.VectorSearchFilter{Metadata: corememory.Metadata{"conversation_id": untouched}},
	}); err != nil {
		t.Fatal(err)
	}
	before := index.Stats()
	if before.SnapshotHits != 1 || before.SnapshotMisses != 0 {
		t.Fatalf("the first read of a resident table = hits %d, misses %d", before.SnapshotHits, before.SnapshotMisses)
	}

	if err := index.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: scope, Shard: shardOf(written),
		Upserts: []component.Artifact{conversationArtifact(written, "b", "alpha again")},
	}); err != nil {
		t.Fatal(err)
	}
	// The write dropped the written shard's table; the warm that follows pays
	// to reload it. Nothing warms the other shard, so a scope-wide invalidation
	// would show up as a load on its next query instead.
	waitForStats(t, index, "the write warming the shard it wrote", func(stats Stats) bool {
		return stats.WarmMisses == 3
	})

	results, err := index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
		Filter: component.VectorSearchFilter{Metadata: corememory.Metadata{"conversation_id": untouched}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "a" {
		t.Fatalf("the untouched shard answered %+v", results)
	}
	after := index.Stats()
	if after.SnapshotMisses != before.SnapshotMisses {
		t.Fatalf("the write forced the other shard to re-read its table: misses %d -> %d",
			before.SnapshotMisses, after.SnapshotMisses)
	}
	if after.SnapshotHits != before.SnapshotHits+1 {
		t.Fatalf("the other shard's read was not served from the cache: hits %d -> %d",
			before.SnapshotHits, after.SnapshotHits)
	}
	if footprint := after.Footprint; footprint.Shards != 2 || footprint.Entries != 3 {
		t.Fatalf("footprint after the write = %+v", footprint)
	}

	// The written shard saw the delta: its table was reloaded by the warm, and
	// the entry the delta added is in it.
	results, err = index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
		Filter: component.VectorSearchFilter{Metadata: corememory.Metadata{"conversation_id": written}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("the written shard answered %+v", results)
	}
}
