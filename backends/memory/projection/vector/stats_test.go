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

// TestStatsCountsAFailedLookupApartFromLoads pins the counter split: a lookup
// that returned an error must not be read as a table the lane served. A load
// and a failed lookup are two outcomes and are counted apart.
func TestStatsCountsAFailedLookupApartFromLoads(t *testing.T) {
	runtime, model := fakeRuntime(t)
	kvStore, ok := kvFor(t).(*storage.WorkspaceKV)
	if !ok {
		t.Fatalf("kvFor returned %T", kvFor(t))
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	// The shard has to exist for the failed read to be a lookup at all: a scope
	// no write reached holds no shard, and a global read of it is empty rather
	// than unreadable.
	writer, err := New(Config{KV: kvStore, Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
	}); err != nil {
		t.Fatal(err)
	}
	index, err := New(Config{KV: unreadableStore{kvStore}, Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "alpha"}); err == nil {
		t.Fatal("a lookup that cannot read its state returned a result")
	}
	stats := index.Stats()
	if stats.ShardErrors != 1 || stats.ShardLoads != 0 {
		t.Fatalf("failed lookup counted as %+v", stats)
	}
	if stats.Resident.Misses != 0 || stats.Resident.Bases != 0 {
		t.Fatalf("a failed lookup left a table resident: %+v", stats.Resident)
	}
}

// TestStatsAttributesThePhasesOfAQuery pins what one query is made of: the
// shard it materialized, the query it embedded, the table it scanned, and what
// the store paid to hand that table over. A second query of the same shard is
// served out of the store's kept base, which is the only resident copy.
func TestStatsAttributesThePhasesOfAQuery(t *testing.T) {
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
	if stats.ShardLoads != 2 || stats.ShardErrors != 0 {
		t.Fatalf("shard loads/errors = %d/%d: each query reads the one shard it names",
			stats.ShardLoads, stats.ShardErrors)
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
	// The rebuild published the table but kept nothing: the value it published
	// was the caller's. The first query decodes it and the store keeps it, so
	// the second is a hit -- one table resident, weighed in its vector payload
	// (three entries of two dimensions).
	if stats.Resident.Bases != 1 || stats.Resident.Bytes != 3*2*4 {
		t.Fatalf("resident = %+v, want the one table the first query decoded", stats.Resident)
	}
	if stats.Resident.Hits != 1 || stats.Resident.Misses != 1 {
		t.Fatalf("serving two queries off one table = hits %d, misses %d",
			stats.Resident.Hits, stats.Resident.Misses)
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
