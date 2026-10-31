package vector

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestSearchReadsOnlyTheShardsTheRequestNames pins the cost of a read against
// what a request asks for. A conversation's question names one shard, so it
// materializes one table and scans one conversation's entries -- not the
// scope's whole derivation, which is what a single tenant-wide table made it
// do. A request that names nothing is the exact whole-scope recall and reads
// every shard, which is the same answer the tenant-wide table gave.
//
// The lane holds no table of its own: what it reads, it reads out of the store
// through MaterializeView, which keeps each shard's base the store decoded.
func TestSearchReadsOnlyTheShardsTheRequestNames(t *testing.T) {
	ctx := context.Background()
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	named, other := "named", "other"
	if err := index.Rebuild(ctx, component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{
			conversationArtifact(named, "a", "alpha"),
			conversationArtifact(named, "b", "alpha again"),
			conversationArtifact(other, "c", "alpha elsewhere"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	results, err := index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
		Filter: component.VectorSearchFilter{Metadata: corememory.Metadata{"conversation_id": named}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("a conversation's shard answered %+v", results)
	}
	namedRead := index.Stats()
	if namedRead.ShardLoads != 1 || namedRead.EntriesScanned != 2 {
		t.Fatalf("the narrowed read loaded %d tables and scanned %d entries, want 1 and 2",
			namedRead.ShardLoads, namedRead.EntriesScanned)
	}

	results, err = index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("the whole-scope read answered %+v", results)
	}
	whole := index.Stats()
	if loads := whole.ShardLoads - namedRead.ShardLoads; loads != 2 {
		t.Fatalf("the whole-scope read loaded %d tables, want both shards", loads)
	}
	if scanned := whole.EntriesScanned - namedRead.EntriesScanned; scanned != 3 {
		t.Fatalf("the whole-scope read scanned %d entries, want every shard's", scanned)
	}
}

// TestRepeatedReadsShareTheStoresBase pins where a table lives now that the
// lane holds none: the store keeps each shard's base decoded, so the second
// query of a shard finds it materialized and the lane pays a lookup rather than
// a decode. A run of queries over one conversation is therefore not a run of
// decodes, which is what a lane materializing per query would cost.
func TestRepeatedReadsShareTheStoresBase(t *testing.T) {
	ctx := context.Background()
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := index.Rebuild(ctx, component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{conversationArtifact("conversation", "a", "alpha")},
	}); err != nil {
		t.Fatal(err)
	}
	request := component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
		Filter: component.VectorSearchFilter{Metadata: corememory.Metadata{"conversation_id": "conversation"}},
	}
	for query := 0; query < 3; query++ {
		if _, err := index.SearchVector(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	resident := index.Stats().Resident
	if resident.Bases != 1 || resident.Misses != 1 || resident.Hits != 2 {
		t.Fatalf("three reads of one table = %+v, want one decode and two hits",
			resident)
	}
}

// TestAWriteAdvancesTheBaseItWrote pins the other half: a delta moves the
// store's kept base forward instead of dropping it, so the write that lands in
// a shard does not force the next query of that shard to decode it again.
func TestAWriteAdvancesTheBaseItWrote(t *testing.T) {
	ctx := context.Background()
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := index.Rebuild(ctx, component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{conversationArtifact("conversation", "a", "alpha")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: scope, Shard: shardOf("conversation"), Projection: "facts",
		Upserts: []component.Artifact{conversationArtifact("conversation", "b", "alpha again")},
	}); err != nil {
		t.Fatal(err)
	}
	// The delta reads the base it applies to -- that is the one decode of the
	// sequence -- and advances the store's copy instead of dropping it.
	if resident := index.Stats().Resident; resident.Misses != 1 {
		t.Fatalf("the write read the base %d times, want the one apply it needed", resident.Misses)
	}
	results, err := index.SearchVector(ctx, component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0}, Limit: 10,
		Filter: component.VectorSearchFilter{Metadata: corememory.Metadata{"conversation_id": "conversation"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("the shard after the write answered %+v", results)
	}
	if resident := index.Stats().Resident; resident.Misses != 1 {
		t.Fatalf("the read after the write decoded a base the store held: %+v", resident)
	}
}
