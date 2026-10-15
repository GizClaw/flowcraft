package vector

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestWriteInvalidatesOnlyTheScopeItWrote pins what a write drops. Two scopes
// hold resident tables; a delta lands in one of them. The written scope's
// table has to be dropped -- the warm that follows reloads it, which is how
// this test sees the drop -- and the other scope's table has to survive: its
// next query is served from the cache, not re-materialized.
//
// Before the lane scoped its invalidation, any write dropped both tables, so a
// host deriving one conversation paid the other conversations' materialization
// again on their next query (9.6s locally for the eval table, per scope).
func TestWriteInvalidatesOnlyTheScopeItWrote(t *testing.T) {
	ctx := context.Background()
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	written := corememory.Scope{RuntimeID: "runtime", UserID: "written"}
	untouched := corememory.Scope{RuntimeID: "runtime", UserID: "untouched"}
	for _, scope := range []corememory.Scope{written, untouched} {
		if err := index.Rebuild(ctx, component.ProjectionRequest{
			Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Both rebuilds warm their own scope: two tables, two background loads.
	waitForStats(t, index, "both rebuilds warming their scope", func(stats Stats) bool {
		return stats.WarmMisses == 2
	})
	if footprint := index.Stats().Footprint; footprint.Scopes != 2 || footprint.Entries != 2 {
		t.Fatalf("footprint after two rebuilds = %+v", footprint)
	}
	if _, err := index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: untouched, Vector: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	before := index.Stats()
	if before.SnapshotHits != 1 || before.SnapshotMisses != 0 {
		t.Fatalf("the first read of a resident table = hits %d, misses %d", before.SnapshotHits, before.SnapshotMisses)
	}

	if err := index.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: written, Upserts: []component.Artifact{vectorArtifact("b", "alpha again")},
	}); err != nil {
		t.Fatal(err)
	}
	// The write dropped the written scope's table; the warm that follows pays
	// to reload it. Nothing warms the other scope, so a global invalidation
	// would show up as a load on its next query instead.
	waitForStats(t, index, "the write warming the scope it wrote", func(stats Stats) bool {
		return stats.WarmMisses == 3
	})

	results, err := index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: untouched, Vector: []float32{1, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "a" {
		t.Fatalf("the untouched scope answered %+v", results)
	}
	after := index.Stats()
	if after.SnapshotMisses != before.SnapshotMisses {
		t.Fatalf("the write forced the other scope to re-read its table: misses %d -> %d",
			before.SnapshotMisses, after.SnapshotMisses)
	}
	if after.SnapshotHits != before.SnapshotHits+1 {
		t.Fatalf("the other scope's read was not served from the cache: hits %d -> %d",
			before.SnapshotHits, after.SnapshotHits)
	}
	if footprint := after.Footprint; footprint.Scopes != 2 || footprint.Entries != 3 {
		t.Fatalf("footprint after the write = %+v", footprint)
	}

	// The written scope saw the delta: its table was reloaded by the warm, and
	// the entry the delta added is in it.
	results, err = index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: written, Vector: []float32{1, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("the written scope answered %+v", results)
	}
}
