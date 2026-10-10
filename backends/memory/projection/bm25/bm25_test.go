package bm25

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	corebm25 "github.com/GizClaw/flowcraft/core/utils/bm25"
)

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
	fragments := map[string]bool{}
	for _, result := range search("世界") {
		if result.Score <= 0 {
			t.Fatalf("query 世界 = %+v, want positive scores", results)
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

func TestBM25DeltaPersistenceIsBoundedAndMatchesFullRebuild(t *testing.T) {
	meter := &bm25Meter{Store: kvFor(t)}
	index, err := New(Config{
		KV: meter, Projection: "facts",
		Thresholds: Thresholds{MaxSegments: 64, MaxDeltaBytes: 1 << 20},
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
		Scope: scope, Upserts: []component.Artifact{changed}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	if meter.written > 8192 {
		t.Fatalf("single upsert wrote %d bytes", meter.written)
	}
	meter.reset()
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, DeleteIDs: []string{artifacts[0].ID}, SourceRevision: "r2",
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

// TestBM25KernelCacheReusesScoringPerSelector pins the lane's kernel cache.
// An unchanged (identity, selector) pair reuses one kernel, and a selector
// change must not: the selector narrows the scored document set, and average
// length and document frequency — the terms a BM25 score is made of — are
// properties of that set.
func TestBM25KernelCacheReusesScoringPerSelector(t *testing.T) {
	index, scope := twoConversationIndex(t)
	search := func(conversation string) []component.Candidate {
		t.Helper()
		results, err := index.Search(context.Background(), component.SearchRequest{
			Scope: scope, Query: "needle",
			Metadata: corememory.Metadata{"conversation_id": conversation},
		})
		if err != nil {
			t.Fatal(err)
		}
		return results
	}

	c1 := search("c1")
	if len(c1) != 1 || c1[0].ID != "alpha" {
		t.Fatalf("conversation c1 results = %s, want only alpha", describeCandidates(c1))
	}
	firstKernel := cachedKernelOf(t, index)

	// The same request must reuse the kernel and answer identically.
	repeat := search("c1")
	if cachedKernelOf(t, index) != firstKernel {
		t.Fatal("identical request rebuilt the kernel")
	}
	if describeCandidates(repeat) != describeCandidates(c1) {
		t.Fatalf("identical request rescored: %s, want %s",
			describeCandidates(repeat), describeCandidates(c1))
	}

	// A different selector selects a different set, so it must not answer
	// from the kernel built for the previous one.
	c2 := search("c2")
	if cachedKernelOf(t, index) == firstKernel {
		t.Fatal("selector change reused the previous kernel")
	}
	if len(c2) != 1 || c2[0].ID != "beta" {
		t.Fatalf("conversation c2 results = %s, want only beta", describeCandidates(c2))
	}

	// Switching back re-selects c1's set and scores identically again.
	if back := search("c1"); describeCandidates(back) != describeCandidates(c1) {
		t.Fatalf("c1 rescored after c2: %s, want %s", describeCandidates(back), describeCandidates(c1))
	}
}

// TestBM25KernelCacheFollowsBuildIdentity pins invalidation: a delta that
// rewrites the projection must produce a new kernel, or the lane keeps
// answering from a build that no longer exists.
func TestBM25KernelCacheFollowsBuildIdentity(t *testing.T) {
	index, err := New(Config{KV: kvFor(t), Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Projection: "facts", Artifacts: []component.Artifact{artifact("alpha", "needle")},
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
	if results := search("needle"); len(results) != 1 || results[0].ID != "alpha" {
		t.Fatalf("results before delta = %s, want only alpha", describeCandidates(results))
	}
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Upserts: []component.Artifact{artifact("beta", "newcomer")}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	// A stale kernel would answer nothing here.
	if results := search("newcomer"); len(results) != 1 || results[0].ID != "beta" {
		t.Fatalf("results after delta = %s, want only beta", describeCandidates(results))
	}
	if results := search("needle"); len(results) != 1 || results[0].ID != "alpha" {
		t.Fatalf("results after delta = %s, want only alpha", describeCandidates(results))
	}
}

// TestBM25KernelCacheConcurrentSearches reads the cache from concurrent
// searches whose selectors alternate, keeping hits and misses overlapped.
func TestBM25KernelCacheConcurrentSearches(t *testing.T) {
	index, scope := twoConversationIndex(t)
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		conversation, want := "c1", "alpha"
		if worker%2 == 1 {
			conversation, want = "c2", "beta"
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 25; i++ {
				results, err := index.Search(context.Background(), component.SearchRequest{
					Scope: scope, Query: "needle",
					Metadata: corememory.Metadata{"conversation_id": conversation},
				})
				if err != nil {
					t.Errorf("conversation %s: %v", conversation, err)
					return
				}
				if len(results) != 1 || results[0].ID != want {
					t.Errorf("conversation %s results = %s, want only %s",
						conversation, describeCandidates(results), want)
					return
				}
			}
		}()
	}
	wait.Wait()
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

func cachedKernelOf(t *testing.T, index *Index) *corebm25.Index {
	t.Helper()
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.cached == nil {
		t.Fatal("no kernel cached")
	}
	return index.cached.kernel
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
