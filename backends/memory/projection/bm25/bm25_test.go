package bm25

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	corebm25 "github.com/GizClaw/flowcraft/core/utils/bm25"
)

// testShard is where artifact()'s conversation lives.
const testShard = "c:conversation"

func TestBM25HappyPathUnicodeAndStableTie(t *testing.T) {
	index, err := New(Config{KV: kvFor(t), Projection: "facts", K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	artifacts := []component.Artifact{
		artifact("b", "GOPHER 世界"),
		artifact("a", "gopher 世界"),
		artifact("c", "unrelated"),
	}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{Scope: scope, Projection: "facts", Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "GoPhEr 世界"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ID != "a" || results[1].ID != "b" || results[0].Score <= 0 {
		t.Fatalf("results = %+v", results)
	}
}

func TestBM25RejectsParameters(t *testing.T) {
	if _, err := New(Config{KV: kvFor(t), Projection: "x", K1: -1}); err == nil {
		t.Fatal("accepted negative k1")
	}
	if _, err := New(Config{KV: kvFor(t), Projection: "x", K1: 1, B: 2}); err == nil {
		t.Fatal("accepted b > 1")
	}
}

// TestBM25ProducerIdentityNamesTheScoringConfiguration guards the identity the
// lane stamps into its manifest. The algorithm version covers the tokenizer the
// persisted terms come from, and k1/b ride along because a delta apply rewrites
// them from live config without moving the manifest digests: without them in
// the identity, a parameter change would be silent.
func TestBM25ProducerIdentityNamesTheScoringConfiguration(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	kvStore := kvFor(t)
	index, err := New(Config{KV: kvStore, Projection: "facts", K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatal(err)
	}
	if version := index.ProducerVersion(); version != "okapi-bm25-v2+k1=1.2,b=0.75" {
		t.Fatalf("producer version = %q", version)
	}
	if _, found, err := index.StoredProducerIdentity(ctx, scope); err != nil || found {
		t.Fatalf("empty lane reported state = %v, %v", found, err)
	}
	if err := index.FullRebuild(ctx, component.ProjectionRequest{
		Scope: scope, Projection: "facts", Artifacts: []component.Artifact{artifact("a", "hello")},
	}); err != nil {
		t.Fatal(err)
	}
	identity, found, err := index.StoredProducerIdentity(ctx, scope)
	if err != nil || !found {
		t.Fatalf("stored identity = %#v, %v, %v", identity, found, err)
	}
	if identity.Version != index.ProducerVersion() || identity.SourceDigest == "" || identity.BuildDigest == "" {
		t.Fatalf("stored identity = %#v, want the version this index writes", identity)
	}
	// A lane built with other scoring parameters writes other state, and the
	// versions differ: that difference is what reports the older state as
	// stale instead of letting the next write adopt the new parameters.
	other, err := New(Config{KV: kvStore, Projection: "facts", K1: 1.5, B: 0.75})
	if err != nil {
		t.Fatal(err)
	}
	if other.ProducerVersion() == index.ProducerVersion() {
		t.Fatalf("scoring change did not change the identity: %q", other.ProducerVersion())
	}
}

func TestBM25EmptyQueryReturnsEmpty(t *testing.T) {
	index, _ := New(Config{KV: kvFor(t), Projection: "facts"})
	results, err := index.Search(context.Background(), component.SearchRequest{
		Scope: corememory.Scope{RuntimeID: "runtime"}, Query: " \t ",
	})
	if err != nil || len(results) != 0 {
		t.Fatalf("empty query = %+v, %v", results, err)
	}
}

// TestBM25MatchingUsesKernelTokens guards the contract between the lane and
// the shared scoring kernel: the lane persists the kernel's tokens
// (core/utils/bm25.Tokenize) and Search tokenizes the query with that same
// splitter, which emits a CJK run as characters plus adjacent bigrams. A
// Chinese document is therefore reachable by the run and by a fragment of it,
// while the lane keeps prefix matching off, so an ASCII fragment still does
// not match a longer word.
func TestBM25MatchingUsesKernelTokens(t *testing.T) {
	index, err := New(Config{KV: kvFor(t), Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := index.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Projection: "facts",
		Artifacts: []component.Artifact{
			artifact("run", "你好世界"),
			artifact("parts", "世界 检索 文档"),
			artifact("ascii", "hello world"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	search := func(query string) []component.Candidate {
		t.Helper()
		results, err := index.Search(context.Background(), component.SearchRequest{
			Scope: scope, Query: query,
		})
		if err != nil {
			t.Fatal(err)
		}
		return results
	}
	ids := func(results []component.Candidate) []string {
		names := make([]string, 0, len(results))
		for _, result := range results {
			names = append(names, result.ID)
		}
		return names
	}

	// The whole run matches, and the document that is the run outranks the one
	// that only shares characters with it.
	results := search("你好世界")
	if len(results) != 2 || results[0].ID != "run" || results[0].Score <= results[1].Score {
		t.Fatalf("query 你好世界 = %+v, want run ahead of parts", results)
	}
	// A fragment matches on its own, including one that never was a whole
	// word of the run: the kernel indexes the run's bigrams, which is what
	// replaces a segmentation dictionary.
	fragmentResults := search("世界")
	fragments := map[string]bool{}
	for _, result := range fragmentResults {
		if result.Score <= 0 {
			t.Fatalf("query 世界 = %+v, want positive scores", fragmentResults)
		}
		fragments[result.ID] = true
	}
	if !fragments["run"] || !fragments["parts"] || len(fragments) != 2 {
		t.Fatalf("query 世界 = %v, want both CJK documents", fragments)
	}
	// Space-separated CJK words still match their terms.
	if results := search("检索 文档"); len(results) != 1 || results[0].ID != "parts" {
		t.Fatalf("query 检索 文档 = %+v, want only parts", results)
	}
	// ASCII keeps whole-word matching: a five-rune prefix of "hello" is not a
	// term of the index and the lane scores prefixes with weight zero.
	if got := ids(search("hello")); len(got) != 1 || got[0] != "ascii" {
		t.Fatalf("query hello = %v, want only ascii", got)
	}
	if got := ids(search("hell")); len(got) != 0 {
		t.Fatalf("query hell = %v, want no hits (prefix matching is off)", got)
	}
}

func TestBM25DatasetFilterPrecedesLimitForEveryDocumentKind(t *testing.T) {
	kinds := []corememory.ContextItemKind{
		corememory.ContextDocumentResource,
		corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk,
		corememory.ContextDocumentSummary,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			index, err := New(Config{KV: kvFor(t), Projection: "documents"})
			if err != nil {
				t.Fatal(err)
			}
			scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
			allowed := artifact("allowed", "needle")
			excluded := artifact("excluded", "needle needle needle needle")
			for _, item := range []*component.Artifact{&allowed, &excluded} {
				item.Kind = component.ArtifactKind(kind)
				item.Metadata["context_kind"] = string(kind)
				item.Metadata["document_id"] = "document-" + item.ID
				item.Metadata["item_id"] = item.ID
			}
			allowed.Metadata["dataset_id"] = "allowed"
			excluded.Metadata["dataset_id"] = "excluded"
			if err := index.Rebuild(context.Background(), component.ProjectionRequest{
				Scope: scope, Projection: "documents", Artifacts: []component.Artifact{allowed, excluded},
			}); err != nil {
				t.Fatal(err)
			}
			results, err := index.Search(context.Background(), component.SearchRequest{
				Scope: scope, Query: "needle", Limit: 1,
				Metadata: corememory.Metadata{"dataset_ids": `["allowed"]`},
			})
			if err != nil || len(results) != 1 || results[0].ID != "allowed" {
				t.Fatalf("results = %+v, %v", results, err)
			}
		})
	}
}

// TestBM25DeleteStopsMatching pins what the bounded-delta test above leaves
// open: it deletes a document but only ever queries words the *new* document
// carries, so a lane that kept the deleted document's terms would pass it.
// Here the deleted document has a term of its own, and it has to stop
// matching while its neighbour keeps matching.
func TestBM25DeleteStopsMatching(t *testing.T) {
	index, err := New(Config{KV: kvFor(t), Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Projection: "facts",
		Artifacts: []component.Artifact{
			artifact("keep", "shared alpha"),
			artifact("gone", "shared omega"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	search := func(query string) []component.Candidate {
		t.Helper()
		results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: query})
		if err != nil {
			t.Fatal(err)
		}
		return results
	}
	if results := search("omega"); len(results) != 1 || results[0].ID != "gone" {
		t.Fatalf("search(omega) = %s, want only gone", describeCandidates(results))
	}
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Shard: testShard, DeleteIDs: []string{"gone"}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	if results := search("omega"); len(results) != 0 {
		t.Fatalf("search(omega) after the delete = %s, want none", describeCandidates(results))
	}
	if results := search("alpha"); len(results) != 1 || results[0].ID != "keep" {
		t.Fatalf("search(alpha) after the delete = %s, want only keep", describeCandidates(results))
	}
}

func TestBM25DeltaPersistenceIsBoundedAndMatchesFullRebuild(t *testing.T) {
	meter := &bm25Meter{Store: kvFor(t)}
	index, err := New(Config{
		KV: meter, Projection: "facts",
		Thresholds: Thresholds{MaxSegments: 64, MinDeltaBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	artifacts := make([]component.Artifact, 1000)
	for i := range artifacts {
		artifacts[i] = artifact(fmt.Sprintf("item-%04d", i), "common original")
	}
	if err := index.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: artifacts,
	}); err != nil {
		t.Fatal(err)
	}
	meter.reset()
	changed := artifact("changed", "common updated")
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Shard: testShard, Upserts: []component.Artifact{changed}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	if meter.written > 8192 {
		t.Fatalf("single upsert wrote %d bytes", meter.written)
	}
	meter.reset()
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Shard: testShard, DeleteIDs: []string{artifacts[0].ID}, SourceRevision: "r2",
	}); err != nil {
		t.Fatal(err)
	}
	if meter.written > 8192 {
		t.Fatalf("single delete wrote %d bytes", meter.written)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "updated"})
	if err != nil || len(results) != 1 || results[0].ID != "changed" {
		t.Fatalf("delta results = %+v, %v", results, err)
	}
	full, err := New(Config{KV: kvFor(t), Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	if err := full.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: append(component.CloneArtifacts(artifacts[1:]), changed),
	}); err != nil {
		t.Fatal(err)
	}
	fullResults, err := full.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "updated"})
	if err != nil || len(fullResults) != 1 || fullResults[0].ID != results[0].ID ||
		fullResults[0].Score != results[0].Score {
		t.Fatalf("full results = %+v, %v; delta = %+v", fullResults, err, results)
	}
}

// TestBM25ShardedSearchEqualsOneKernelOverTheScoringSet pins the two-pass
// contract. A sharded read has to answer like one kernel over every record the
// selector admits: BM25's document count, average length and document frequency
// belong to that set, and a shard scored against its own counts would weigh the
// same term differently from the shard next to it. The cases cover the plans a
// read can take: one named conversation, the other, and no selector at all,
// which fans out over every shard the lane holds.
func TestBM25ShardedSearchEqualsOneKernelOverTheScoringSet(t *testing.T) {
	ctx := context.Background()
	const k1, b = 1.2, 0.75
	index, err := New(Config{KV: kvFor(t), Projection: "facts", K1: k1, B: b})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	first := artifact("a1", "needle needle alpha")
	first.Metadata["conversation_id"] = "c1"
	second := artifact("a2", "needle beta")
	second.Metadata["conversation_id"] = "c2"
	third := artifact("a3", "needle needle needle gamma")
	third.Metadata["conversation_id"] = "c2"
	if err := index.FullRebuild(ctx, component.ProjectionRequest{
		Scope: scope, Projection: "facts",
		Artifacts: []component.Artifact{first, second, third},
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		metadata corememory.Metadata
	}{
		{name: "conversation c1", metadata: corememory.Metadata{"conversation_id": "c1"}},
		{name: "conversation c2", metadata: corememory.Metadata{"conversation_id": "c2"}},
		{name: "every conversation", metadata: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector, err := projectionstore.ParseSelector(test.metadata)
			if err != nil {
				t.Fatal(err)
			}
			shards, err := index.searchShards(ctx, scope, selector)
			if err != nil {
				t.Fatal(err)
			}
			want := referenceSearch(t, ctx, index, scope, shards, selector, k1, b, "needle", 0)
			got, err := index.Search(ctx, component.SearchRequest{
				Scope: scope, Query: "needle", Metadata: test.metadata,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(want) == 0 {
				t.Fatal("the reference kernel matched nothing; the fixture is wrong")
			}
			if len(got) != len(want) {
				t.Fatalf("search = %s, want %d results", describeCandidates(got), len(want))
			}
			for i := range want {
				if got[i].ID != want[i].ID || got[i].Score != want[i].Score {
					t.Fatalf("result %d = %s, want %s=%.17g",
						i, describeCandidates(got), want[i].ID, want[i].Score)
				}
			}
		})
	}
}

// TestBM25GlobalRecallKeepsTheSingleTableTopK pins the truncation end of the
// two-pass read: each shard contributes its own best records and the merge
// keeps the top limit of the union. With a limit smaller than the shard count
// a read that truncated per shard first would still be right only by accident;
// here one conversation outranks the other, so the limit has to cut across
// shards in score order.
func TestBM25GlobalRecallKeepsTheSingleTableTopK(t *testing.T) {
	ctx := context.Background()
	const k1, b = 1.2, 0.75
	index, err := New(Config{KV: kvFor(t), Projection: "facts", K1: k1, B: b})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	weak := artifact("weak", "needle")
	strong := artifact("strong", "needle needle needle needle")
	strong.Metadata["conversation_id"] = "c2"
	if err := index.FullRebuild(ctx, component.ProjectionRequest{
		Scope: scope, Projection: "facts", Artifacts: []component.Artifact{weak, strong},
	}); err != nil {
		t.Fatal(err)
	}
	selector, err := projectionstore.ParseSelector(nil)
	if err != nil {
		t.Fatal(err)
	}
	shards, err := index.searchShards(ctx, scope, selector)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 2} {
		want := referenceSearch(t, ctx, index, scope, shards, selector, k1, b, "needle", limit)
		got, err := index.Search(ctx, component.SearchRequest{Scope: scope, Query: "needle", Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) || len(got) == 0 {
			t.Fatalf("limit %d search = %s, want %d results", limit, describeCandidates(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID || got[i].Score != want[i].Score {
				t.Fatalf("limit %d result %d = %s, want %s=%.17g",
					limit, i, describeCandidates(got), want[i].ID, want[i].Score)
			}
		}
	}
	if first, err := index.Search(ctx, component.SearchRequest{Scope: scope, Query: "needle", Limit: 1}); err != nil ||
		len(first) != 1 || first[0].ID != "strong" {
		t.Fatalf("limit 1 = %s, %v, want the record with the strongest term frequency", describeCandidates(first), err)
	}
}

// TestBM25DeltaLandsOnlyInItsShard pins the write path's isolation: a delta
// names the shard it lands in, so an upsert or a delete in one conversation
// must not touch its neighbours. The global read afterwards is what proves the
// neighbours are still there.
func TestBM25DeltaLandsOnlyInItsShard(t *testing.T) {
	ctx := context.Background()
	index, err := New(Config{KV: kvFor(t), Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	alpha := artifact("alpha", "shared alpha")
	beta := artifact("beta", "shared beta")
	beta.Metadata["conversation_id"] = "other"
	if err := index.FullRebuild(ctx, component.ProjectionRequest{
		Scope: scope, Projection: "facts", Artifacts: []component.Artifact{alpha, beta},
	}); err != nil {
		t.Fatal(err)
	}
	gamma := artifact("gamma", "omega")
	gamma.Metadata["conversation_id"] = "other"
	if err := index.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: scope, Shard: "c:other", Upserts: []component.Artifact{gamma},
		SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	if results, err := index.Search(ctx, component.SearchRequest{
		Scope: scope, Query: "omega", Metadata: corememory.Metadata{"conversation_id": "conversation"},
	}); err != nil || len(results) != 0 {
		t.Fatalf("the neighbour conversation matched the upsert: %+v, %v", results, err)
	}
	if results, err := index.Search(ctx, component.SearchRequest{
		Scope: scope, Query: "omega", Metadata: corememory.Metadata{"conversation_id": "other"},
	}); err != nil || len(results) != 1 || results[0].ID != "gamma" {
		t.Fatalf("the addressed conversation missed its upsert: %+v, %v", results, err)
	}
	// A delete names its shard too: the other conversation keeps its record.
	if err := index.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: scope, Shard: testShard, DeleteIDs: []string{"alpha"},
		SourceRevision: "r2",
	}); err != nil {
		t.Fatal(err)
	}
	if results, err := index.Search(ctx, component.SearchRequest{Scope: scope, Query: "shared"}); err != nil ||
		len(results) != 1 || results[0].ID != "beta" {
		t.Fatalf("delete in one shard touched another: %+v, %v", results, err)
	}
}

// TestBM25RebuildPrunesAShardItsArtifactsNoLongerCover pins replacement, not
// addition: a scope-wide rebuild publishes the shards its artifact set covers,
// and a conversation the set no longer holds must stop answering instead of
// keeping its last index.
func TestBM25RebuildPrunesAShardItsArtifactsNoLongerCover(t *testing.T) {
	ctx := context.Background()
	index, err := New(Config{KV: kvFor(t), Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	kept := artifact("kept", "needle")
	gone := artifact("gone", "needle")
	gone.Metadata["conversation_id"] = "removed"
	if err := index.FullRebuild(ctx, component.ProjectionRequest{
		Scope: scope, Projection: "facts", Artifacts: []component.Artifact{kept, gone},
	}); err != nil {
		t.Fatal(err)
	}
	if shards, err := index.store.Shards(ctx, scope, "facts"); err != nil || len(shards) != 2 {
		t.Fatalf("shards = %v, %v, want both conversations", shards, err)
	}
	if err := index.FullRebuild(ctx, component.ProjectionRequest{
		Scope: scope, Projection: "facts", Artifacts: []component.Artifact{kept},
	}); err != nil {
		t.Fatal(err)
	}
	if shards, err := index.store.Shards(ctx, scope, "facts"); err != nil || len(shards) != 1 || shards[0] != testShard {
		t.Fatalf("shards after the rebuild = %v, %v, want only %s", shards, err, testShard)
	}
	if results, err := index.Search(ctx, component.SearchRequest{
		Scope: scope, Query: "needle", Metadata: corememory.Metadata{"conversation_id": "removed"},
	}); err != nil || len(results) != 0 {
		t.Fatalf("the pruned conversation still answers: %+v, %v", results, err)
	}
}

// TestBM25ConcurrentSearchesAgreeWithSequentialOnes replaces what the old
// per-selector kernel cache test guarded. The lane keeps no kernel between
// requests, so concurrent reads cannot race on one; what still has to hold is
// that the answer does not depend on how many readers asked at once.
func TestBM25ConcurrentSearchesAgreeWithSequentialOnes(t *testing.T) {
	index, scope := twoConversationIndex(t)
	queries := []component.SearchRequest{
		{Scope: scope, Query: "needle", Metadata: corememory.Metadata{"conversation_id": "c1"}},
		{Scope: scope, Query: "needle", Metadata: corememory.Metadata{"conversation_id": "c2"}},
		{Scope: scope, Query: "needle"},
	}
	want := make([]string, len(queries))
	for i, request := range queries {
		results, err := index.Search(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = describeCandidates(results)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 25; i++ {
				request := queries[i%len(queries)]
				results, err := index.Search(context.Background(), request)
				if err != nil {
					t.Errorf("search: %v", err)
					return
				}
				if got := describeCandidates(results); got != want[i%len(queries)] {
					t.Errorf("concurrent search = %s, want %s", got, want[i%len(queries)])
					return
				}
			}
		}()
	}
	wait.Wait()
}

// referenceSearch answers one query the way an unsharded lane would: a single
// kernel over every record the selector admits in the planned shards.
func referenceSearch(
	t *testing.T,
	ctx context.Context,
	index *Index,
	scope corememory.Scope,
	shards []string,
	selector projectionstore.Selector,
	k1, b float64,
	query string,
	limit int,
) []corebm25.Result {
	t.Helper()
	documents := make([]corebm25.TermDoc, 0)
	for _, shard := range shards {
		build, found, err := index.materialize(ctx, scope, shard)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			continue
		}
		for _, document := range build.Documents {
			if !selector.Matches(document.Address) {
				continue
			}
			documents = append(documents, corebm25.TermDoc{ID: document.ID, Text: document.Terms})
		}
	}
	kernel, err := corebm25.NewFromTerms(documents,
		corebm25.WithK1(k1), corebm25.WithB(b), corebm25.WithPrefixWeight(0))
	if err != nil {
		t.Fatal(err)
	}
	return kernel.SearchTerms(corebm25.Tokenize(query), limit)
}

// twoConversationIndex builds one fact per conversation selector, with
// different term frequencies so the two scored sets differ.
func twoConversationIndex(t *testing.T) (*Index, corememory.Scope) {
	t.Helper()
	index, err := New(Config{KV: kvFor(t), Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	alpha := artifact("alpha", "needle")
	alpha.Metadata["conversation_id"] = "c1"
	beta := artifact("beta", "needle needle needle")
	beta.Metadata["conversation_id"] = "c2"
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Projection: "facts", Artifacts: []component.Artifact{alpha, beta},
	}); err != nil {
		t.Fatal(err)
	}
	return index, scope
}

func describeCandidates(results []component.Candidate) string {
	var description strings.Builder
	for _, result := range results {
		fmt.Fprintf(&description, "%s=%.17g;", result.ID, result.Score)
	}
	return description.String()
}

type bm25Meter struct {
	storage.Store
	mu      sync.Mutex
	written int
}

func (meter *bm25Meter) Put(ctx context.Context, key string, data []byte) error {
	meter.mu.Lock()
	meter.written += len(data)
	meter.mu.Unlock()
	return meter.Store.Put(ctx, key, data)
}

func (meter *bm25Meter) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	meter.mu.Lock()
	meter.written += len(data)
	meter.mu.Unlock()
	return meter.Store.(storage.PutIfAbsentStore).PutIfAbsent(ctx, key, data)
}

func (meter *bm25Meter) reset() {
	meter.mu.Lock()
	meter.written = 0
	meter.mu.Unlock()
}

func kvFor(t *testing.T) storage.Store {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	return kvStore
}

func artifact(id, text string) component.Artifact {
	return component.Artifact{
		Kind: "fact", ID: id,
		Content:  coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}},
		Sources:  []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "source-" + id}},
		Metadata: corememory.Metadata{"conversation_id": "conversation"},
	}
}
