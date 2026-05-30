package bbh

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/contract"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/coder/hnsw"
	"github.com/dgraph-io/badger/v4"
)

func TestNewValidationAndClosedIndexErrors(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil workspace should fail")
	}
	if _, err := New(sdkworkspace.NewMemWorkspace()); err == nil {
		t.Fatal("workspace without Root should fail")
	}
	if _, err := New(rootedWorkspace(t, filepath.Join(t.TempDir(), "missing-config")), WithConfigFilePath("missing.yaml")); err == nil {
		t.Fatal("bad config in New should fail")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, bleveDir), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(rootedWorkspace(t, root)); err == nil {
		t.Fatal("bleve root file should fail")
	}
	idx := openInternalIndex(t, t.TempDir())
	if !idx.SupportsFilter(retrieval.Filter{}) {
		t.Fatal("SupportsFilter should be true")
	}
	caps := idx.Capabilities()
	if !caps.BM25 || !caps.Vector || !caps.Hybrid || !caps.NativeDeleteByFilter {
		t.Fatalf("unexpected caps: %+v", caps)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{ID: "x"}}); err == nil {
		t.Fatal("upsert after close should fail")
	}
	if _, _, err := idx.Get(context.Background(), "ns", "x"); err == nil {
		t.Fatal("get after close should fail")
	}
	if _, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{QueryText: "x"}); err == nil {
		t.Fatal("search after close should fail")
	}
}

func TestOpenBleveStatError(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file-parent")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openBleve(filepath.Join(parent, "child"), BleveConfig{Analyzer: "standard"}); err == nil {
		t.Fatal("openBleve under file parent should fail")
	}
}

func TestEnsureShardOpenBleveError(t *testing.T) {
	idx := openInternalIndex(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(idx.root, bleveDir, safeToken("broken")), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Search(context.Background(), "broken", retrieval.SearchRequest{QueryText: "x"}); err == nil {
		t.Fatal("broken bleve shard should fail")
	}
}

func TestNewBadgerOpenError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, bleveDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, hnswDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, badgerDir), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(rootedWorkspace(t, root)); err == nil {
		t.Fatal("badger file should fail")
	}
}

func TestIndexManagementAndProjectionPaths(t *testing.T) {
	ctx := context.Background()
	idx := openInternalIndex(t, t.TempDir(), WithConfig(Config{
		SearchOverfetch: 2,
		HNSW:            HNSWConfig{FlushInterval: Duration{Duration: time.Hour}},
	}))
	docs := []retrieval.Doc{
		{
			ID:        "a",
			Content:   "alpha coffee",
			Vector:    []float32{1, 0, 0},
			Metadata:  map[string]any{"kind": "keep", "score": 1, "tags": []any{"red", "blue"}},
			Timestamp: time.Unix(10, 0).UTC(),
		},
		{
			ID:        "b",
			Content:   "beta coffee",
			Vector:    []float32{0, 1, 0},
			Metadata:  map[string]any{"kind": "drop", "score": 2, "tags": []any{"green"}},
			Timestamp: time.Unix(20, 0).UTC(),
		},
		{
			ID:        "c",
			Content:   "gamma tea",
			Vector:    []float32{0, 0, 1},
			Metadata:  map[string]any{"kind": "keep", "score": 3, "tags": []any{"blue"}},
			Timestamp: time.Unix(30, 0).UTC(),
		},
	}
	if err := idx.Upsert(ctx, "ns", docs); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: ""}}); err == nil {
		t.Fatal("empty id should fail")
	}
	if _, _, err := idx.Get(ctx, "ns", "missing"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := idx.Get(ctx, "ns", "a")
	if err != nil || !ok || got.ID != "a" {
		t.Fatalf("Get a = %+v %v %v", got, ok, err)
	}
	count, err := idx.Count(ctx, "ns", retrieval.Filter{Eq: map[string]any{"kind": "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count keep = %d", count)
	}
	page, err := idx.List(ctx, "ns", retrieval.ListRequest{
		PageSize:   2,
		OrderBy:    retrieval.OrderByTimestampAsc,
		WithVector: true,
		Project:    []string{"content", "timestamp", "vector", "metadata.kind"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "a" || page.Items[1].ID != "b" || page.NextPageToken == "" || page.Total != 3 {
		t.Fatalf("page = %+v", page)
	}
	if page.Items[0].Content == "" || len(page.Items[0].Vector) == 0 || page.Items[0].Metadata["kind"] != "keep" {
		t.Fatalf("projection failed: %+v", page.Items[0])
	}
	page2, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: 2, PageToken: page.NextPageToken, OrderBy: retrieval.OrderByTimestampAsc})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Items) != 1 || page2.Items[0].ID != "c" {
		t.Fatalf("page2 = %+v", page2)
	}
	emptyPage, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: 2, PageToken: page.NextPageToken, OrderBy: retrieval.OrderByIDAsc})
	if err == nil || emptyPage != nil {
		t.Fatalf("mismatched page token should fail, got page=%+v err=%v", emptyPage, err)
	}
	allDesc, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: defaultMaxListPageSize + 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(allDesc.Items) != 3 || allDesc.Items[0].ID != "c" {
		t.Fatalf("default desc page = %+v", allDesc)
	}
	byID, err := idx.List(ctx, "ns", retrieval.ListRequest{OrderBy: retrieval.OrderByIDAsc})
	if err != nil {
		t.Fatal(err)
	}
	if len(byID.Items) != 3 || byID.Items[0].ID != "a" {
		t.Fatalf("id asc page = %+v", byID)
	}
	ids := iterIDs(t, idx, "ns", "", 2)
	if !slices.Equal(ids, []string{"a", "b"}) {
		t.Fatalf("first iterate ids = %v", ids)
	}
	nextDocs, next, err := idx.Iterate(ctx, "ns", "b", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(nextDocs) != 1 || nextDocs[0].ID != "c" || next != "" {
		t.Fatalf("second iterate = %+v next=%q", nextDocs, next)
	}
	if _, _, err := idx.Iterate(ctx, "ns", "z", 1); err != nil {
		t.Fatal(err)
	}
	if docs, next, err := idx.Iterate(ctx, "ns", "", 0); err != nil || len(docs) != 3 || next != "" {
		t.Fatalf("default iterate = %d next=%q err=%v", len(docs), next, err)
	}
	deleted, err := idx.DeleteByFilter(ctx, "ns", retrieval.Filter{})
	if !errors.Is(err, retrieval.ErrEmptyDeleteFilter) || deleted != 0 {
		t.Fatalf("empty delete = %d %v", deleted, err)
	}
	deleted, err = idx.DeleteByFilter(ctx, "ns", retrieval.Filter{Eq: map[string]any{"kind": "drop"}})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d", deleted)
	}
	if err := idx.Delete(ctx, "ns", []string{"missing"}); err != nil {
		t.Fatal(err)
	}
	deleted, err = idx.DeleteByFilter(ctx, "ns", retrieval.Filter{ContainsAny: map[string][]any{"tags": {"blue"}}})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted blue = %d", deleted)
	}
	if err := idx.Drop(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Drop(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
}

func TestAdditionalIndexBranches(t *testing.T) {
	ctx := context.Background()
	idx := openInternalIndex(t, t.TempDir())
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{
		{ID: "a", Content: "same", Vector: []float32{1, 0}, Timestamp: time.Unix(1, 0)},
		{ID: "b", Content: "same", Vector: []float32{0, 1}, Timestamp: time.Unix(1, 0)},
		{ID: "c", Content: "same", Vector: []float32{1, 1}, Timestamp: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "same", Vector: []float32{1, 0}, Timestamp: time.Unix(3, 0)}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "no vector", Timestamp: time.Unix(4, 0)}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, "ns", nil); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, "", []string{"a"}); err == nil {
		t.Fatal("delete empty namespace should fail")
	}
	if err := idx.Delete(ctx, "ns", []string{""}); err != nil {
		t.Fatal(err)
	}
	desc, err := idx.List(ctx, "ns", retrieval.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Items) != 3 || desc.Items[0].ID != "a" || desc.Items[1].ID != "c" || desc.Items[2].ID != "b" {
		t.Fatalf("desc tie ordering = %+v", desc.Items)
	}
	asc, err := idx.List(ctx, "ns", retrieval.ListRequest{OrderBy: retrieval.OrderByTimestampAsc})
	if err != nil {
		t.Fatal(err)
	}
	if len(asc.Items) != 3 || asc.Items[0].ID != "b" || asc.Items[1].ID != "c" || asc.Items[2].ID != "a" {
		t.Fatalf("asc tie ordering = %+v", asc.Items)
	}
	empty, err := idx.List(ctx, "ns", retrieval.ListRequest{PageToken: "eyJvIjo5OTl9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Items) != 0 || empty.Total != 3 {
		t.Fatalf("offset past end = %+v", empty)
	}
	if _, err := idx.Count(ctx, "ns", retrieval.Filter{}); err != nil {
		t.Fatal(err)
	}
	deleted, err := idx.DeleteByFilter(ctx, "ns", retrieval.Filter{Eq: map[string]any{"missing": "value"}})
	if err != nil || deleted != 0 {
		t.Fatalf("delete no matches = %d %v", deleted, err)
	}
	if _, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "same", QueryVector: []float32{1, 0, 0}, TopK: 2}); err == nil {
		t.Fatal("hybrid vector dimension mismatch should fail")
	}
	vec, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryVector: []float32{0, 1}, TopK: 1, MinScore: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(vec.Hits) != 0 {
		t.Fatalf("vector minscore hits = %+v", vec.Hits)
	}
	if _, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "same", TopK: 1}); err != nil {
		t.Fatal(err)
	}
	if got := idx.searchWindow(0, 5); got != 5 {
		t.Fatalf("searchWindow default topK = %d", got)
	}
}

func TestTieOrderingAndClosedManagementBranches(t *testing.T) {
	ctx := context.Background()
	idx := openInternalIndex(t, t.TempDir())
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{
		{ID: "a", Content: "tie", Timestamp: time.Unix(1, 0)},
		{ID: "b", Content: "tie", Timestamp: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	asc, err := idx.List(ctx, "ns", retrieval.ListRequest{OrderBy: retrieval.OrderByTimestampAsc})
	if err != nil {
		t.Fatal(err)
	}
	if len(asc.Items) != 2 || asc.Items[0].ID != "a" || asc.Items[1].ID != "b" {
		t.Fatalf("asc tie = %+v", asc.Items)
	}
	desc, err := idx.List(ctx, "ns", retrieval.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Items) != 2 || desc.Items[0].ID != "b" || desc.Items[1].ID != "a" {
		t.Fatalf("desc tie = %+v", desc.Items)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.List(ctx, "ns", retrieval.ListRequest{}); err == nil {
		t.Fatal("List after close should fail")
	}
	if _, err := idx.Count(ctx, "ns", retrieval.Filter{}); err == nil {
		t.Fatal("Count after close should fail")
	}
	if err := idx.Drop(ctx, "ns"); err == nil {
		t.Fatal("Drop after close should fail")
	}
	if err := idx.Delete(ctx, "ns", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.DeleteByFilter(ctx, "ns", retrieval.Filter{Eq: map[string]any{"x": 1}}); err == nil {
		t.Fatal("DeleteByFilter after close should fail")
	}
}

func TestUpsertDimensionMismatchError(t *testing.T) {
	idx := openInternalIndex(t, t.TempDir())
	if err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{ID: "a", Vector: []float32{1, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{ID: "bad-dim", Vector: []float32{1, 0}}}); err == nil {
		t.Fatal("upsert dimension mismatch should fail")
	}
	if _, ok, err := idx.Get(context.Background(), "ns", "bad-dim"); err != nil || ok {
		t.Fatalf("dimension mismatch persisted rejected doc: ok=%v err=%v", ok, err)
	}
	if got := idx.shards["ns"].graph.Len(); got != 1 {
		t.Fatalf("dimension mismatch mutated graph len=%d, want 1", got)
	}
}

func TestUpsertMixedVectorBatchIsRejectedWithoutSideEffects(t *testing.T) {
	idx := openInternalIndex(t, t.TempDir())
	err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{
		{ID: "a", Vector: []float32{1, 0, 0}},
		{ID: "bad-dim", Vector: []float32{1, 0}},
	})
	if err == nil {
		t.Fatal("mixed vector dimensions should fail")
	}
	if _, ok, err := idx.Get(context.Background(), "ns", "a"); err != nil || ok {
		t.Fatalf("mixed vector batch persisted first doc: ok=%v err=%v", ok, err)
	}
	if sh, ok := idx.shards["ns"]; ok && sh.graph.Len() != 0 {
		t.Fatalf("mixed vector batch mutated graph len=%d, want 0", sh.graph.Len())
	}
}

func TestRemoveIfExistsError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "non-empty")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeIfExists(dir); err == nil {
		t.Fatal("remove non-empty directory should fail")
	}
}

func TestContextCancellationAcrossMethods(t *testing.T) {
	idx := openInternalIndex(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "x"}}); err == nil {
		t.Fatal("Upsert canceled should fail")
	}
	if _, _, err := idx.Get(ctx, "ns", "x"); err == nil {
		t.Fatal("Get canceled should fail")
	}
	if _, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "x"}); err == nil {
		t.Fatal("Search canceled should fail")
	}
	if _, err := idx.List(ctx, "ns", retrieval.ListRequest{}); err == nil {
		t.Fatal("List canceled should fail")
	}
	if _, err := idx.Count(ctx, "ns", retrieval.Filter{}); err == nil {
		t.Fatal("Count canceled should fail")
	}
	if _, err := idx.DeleteByFilter(ctx, "ns", retrieval.Filter{Eq: map[string]any{"x": 1}}); err == nil {
		t.Fatal("DeleteByFilter canceled should fail")
	}
	if err := idx.Drop(ctx, "ns"); err == nil {
		t.Fatal("Drop canceled should fail")
	}
	if _, _, err := idx.Iterate(ctx, "ns", "", 1); err == nil {
		t.Fatal("Iterate canceled should fail")
	}
	if err := idx.Delete(ctx, "ns", []string{"x"}); err == nil {
		t.Fatal("Delete canceled should fail")
	}
}

func TestBadgerJSONAndUpsertErrorPaths(t *testing.T) {
	ctx := context.Background()
	idx := openInternalIndex(t, t.TempDir())
	if err := idx.Upsert(ctx, "ns", nil); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "bad", Metadata: map[string]any{"fn": func() {}}}}); err == nil {
		t.Fatal("unmarshalable metadata should fail")
	}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "zero-ts", Content: "zero timestamp"}}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := idx.Get(ctx, "ns", "zero-ts")
	if err != nil || !ok || got.Timestamp.IsZero() {
		t.Fatalf("zero timestamp doc = %+v ok=%v err=%v", got, ok, err)
	}
	if err := idx.db.Update(func(txn *badger.Txn) error {
		return txn.Set(docKey("ns", "bad-json"), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := idx.Get(ctx, "ns", "bad-json"); err == nil {
		t.Fatal("bad json Get should fail")
	}
	if _, err := idx.List(ctx, "ns", retrieval.ListRequest{}); err == nil {
		t.Fatal("bad json List should fail")
	}
	if _, err := idx.Count(ctx, "ns", retrieval.Filter{}); err == nil {
		t.Fatal("bad json Count should fail")
	}
	if _, err := idx.DeleteByFilter(ctx, "ns", retrieval.Filter{Eq: map[string]any{"x": 1}}); err == nil {
		t.Fatal("bad json DeleteByFilter should fail")
	}
	if _, _, err := idx.Iterate(ctx, "ns", "", 1); err == nil {
		t.Fatal("bad json Iterate should fail")
	}
}

func TestInternalRebuildAndSearchErrorPaths(t *testing.T) {
	ctx := context.Background()
	idx := openInternalIndex(t, t.TempDir())
	if err := idx.putDoc("ns", retrieval.Doc{ID: "a", Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.putDoc("ns", retrieval.Doc{ID: "b", Vector: []float32{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	sh, err := idx.ensureShard("ns")
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuildGraphLocked("ns", sh); err == nil {
		t.Fatal("rebuild with mixed vector dimensions should fail")
	}

	idx2 := openInternalIndex(t, t.TempDir())
	if err := idx2.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "alpha", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	sh2, err := idx2.ensureShard("ns")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := idx2.searchTextLocked(canceled, sh2, retrieval.SearchRequest{QueryText: "alpha", TopK: 1}); err == nil {
		t.Fatal("canceled bleve search should fail")
	}
	if err := idx2.db.Update(func(txn *badger.Txn) error {
		return txn.Set(docKey("ns", "a"), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := idx2.searchVectorLocked(sh2, retrieval.SearchRequest{QueryVector: []float32{1, 0}, TopK: 1}); err == nil {
		t.Fatal("vector search with bad doc json should fail")
	}
}

func TestExistingIndexCorruptGraphAndAutoFlush(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openInternalIndex(t, dir, WithConfig(Config{
		HNSW: HNSWConfig{FlushInterval: Duration{Duration: 10 * time.Millisecond}},
	}))
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "alpha", Vector: []float32{1, 0}, Timestamp: time.Unix(1, 0)}}); err != nil {
		t.Fatal(err)
	}
	graphPath := filepath.Join(dir, hnswDir, safeToken("ns")+hnswGraphExt)
	deadline := time.Now().Add(time.Second)
	for {
		info, err := os.Stat(graphPath)
		if err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("graph was not auto-flushed: stat=%v info=%v", err, info)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openInternalIndex(t, dir)
	if _, err := reopened.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", QueryVector: []float32{1, 0}, TopK: 1}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graphPath, []byte("not a graph"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := openInternalIndex(t, dir)
	if _, err := corrupt.Search(ctx, "ns", retrieval.SearchRequest{QueryVector: []float32{1, 0}, TopK: 1}); err == nil {
		t.Fatal("corrupt graph should fail when shard opens")
	}
}

func TestPinnedHNSWReplacePathPanics(t *testing.T) {
	g := hnsw.NewGraph[string]()
	g.Rng = rand.New(rand.NewSource(0))
	g.Add(hnsw.MakeNode("doc-000", []float32{1, 1, 1}))
	g.Add(hnsw.MakeNode("doc-001", []float32{2, 2, 2}))

	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		g.Add(hnsw.MakeNode("doc-000", []float32{20, 30, 40}))
	}()
	if panicValue == nil {
		t.Fatal("expected coder/hnsw replace path to panic for pinned v0.6.1 repro")
	}
}

func TestBBHDoesNotUsePanicProneHNSWReplacePath(t *testing.T) {
	ctx := context.Background()
	idx := openInternalIndex(t, t.TempDir(), WithConfig(Config{
		HNSW: HNSWConfig{FlushInterval: Duration{Duration: time.Hour}},
	}))
	mustNotPanic(t, func() {
		if err := idx.Upsert(ctx, "ns", []retrieval.Doc{
			{ID: "doc-000", Content: "first", Vector: []float32{1, 1, 1}, Timestamp: time.Unix(1, 0)},
			{ID: "doc-001", Content: "second", Vector: []float32{2, 2, 2}, Timestamp: time.Unix(2, 0)},
		}); err != nil {
			t.Fatal(err)
		}
		if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{
			ID:        "doc-000",
			Content:   "updated",
			Vector:    []float32{20, 30, 40},
			Timestamp: time.Unix(3, 0),
		}}); err != nil {
			t.Fatal(err)
		}
	})
	resp, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryVector: []float32{20, 30, 40}, TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "doc-000" || resp.Hits[0].Doc.Content != "updated" {
		t.Fatalf("updated vector hits = %+v", resp.Hits)
	}
}

func TestSearchModesAndErrors(t *testing.T) {
	ctx := context.Background()
	idx := openInternalIndex(t, t.TempDir(), WithConfig(Config{SearchOverfetch: 1}))
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{
		{ID: "a", Content: "alpha coffee", Vector: []float32{1, 0}, Metadata: map[string]any{"kind": "keep"}, Timestamp: time.Unix(1, 0)},
		{ID: "b", Content: "beta tea", Vector: []float32{0, 1}, Metadata: map[string]any{"kind": "drop"}, Timestamp: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Search(ctx, "ns", retrieval.SearchRequest{}); !errors.Is(err, retrieval.ErrNoQuery) {
		t.Fatalf("no query err = %v", err)
	}
	if _, err := idx.Search(ctx, "ns", retrieval.SearchRequest{SparseVec: map[string]float32{"x": 1}}); err == nil {
		t.Fatal("sparse-only search should fail")
	}
	if _, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "coffee", SparseVec: map[string]float32{"x": 1}}); err == nil {
		t.Fatal("text+sparse search should fail")
	}
	if _, err := idx.Search(ctx, "", retrieval.SearchRequest{QueryText: "coffee"}); err == nil {
		t.Fatal("empty namespace should fail")
	}
	text, err := idx.Search(ctx, "ns", retrieval.SearchRequest{
		QueryText: "coffee",
		TopK:      5,
		Filter:    retrieval.Filter{Eq: map[string]any{"kind": "keep"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(text.Hits) != 1 || text.Hits[0].Doc.ID != "a" || text.Hits[0].Scores["bm25"] == 0 {
		t.Fatalf("text hits = %+v", text.Hits)
	}
	filteredText, err := idx.Search(ctx, "ns", retrieval.SearchRequest{
		QueryText: "coffee",
		TopK:      5,
		Filter:    retrieval.Filter{Eq: map[string]any{"kind": "missing"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filteredText.Hits) != 0 {
		t.Fatalf("filtered text hits = %+v", filteredText.Hits)
	}
	vec, err := idx.Search(ctx, "ns", retrieval.SearchRequest{
		QueryVector: []float32{1, 0},
		TopK:        1,
		MinScore:    0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vec.Hits) != 1 || vec.Hits[0].Doc.ID != "a" || vec.Hits[0].Scores["cos"] == 0 {
		t.Fatalf("vector hits = %+v", vec.Hits)
	}
	filteredVec, err := idx.Search(ctx, "ns", retrieval.SearchRequest{
		QueryVector: []float32{1, 0},
		TopK:        1,
		Filter:      retrieval.Filter{Eq: map[string]any{"kind": "missing"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filteredVec.Hits) != 0 {
		t.Fatalf("filtered vector hits = %+v", filteredVec.Hits)
	}
	hybrid, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "coffee", QueryVector: []float32{1, 0}, TopK: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hybrid.Hits) == 0 {
		t.Fatal("hybrid should return hits")
	}
	if _, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryVector: []float32{1, 0, 0}}); err == nil {
		t.Fatal("dimension mismatch should fail")
	}
	none, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "coffee", TopK: 1, MinScore: 1e9})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Hits) != 0 {
		t.Fatalf("minscore hits = %+v", none.Hits)
	}
}

func TestProjectDocVariants(t *testing.T) {
	d := retrieval.Doc{
		ID:           "id",
		Content:      "content",
		Vector:       []float32{1},
		SparseVector: map[string]float32{"x": 1},
		Metadata:     map[string]any{"a": 1},
		Timestamp:    time.Unix(1, 0),
	}
	if got := projectDoc(d, nil, false); len(got.Vector) != 0 || len(got.SparseVector) != 0 {
		t.Fatalf("without vector = %+v", got)
	}
	got := projectDoc(d, []string{"metadata", "sparse_vector"}, true)
	if got.Metadata["a"] != 1 || got.SparseVector["x"] != 1 || got.Content != "" {
		t.Fatalf("project metadata/sparse = %+v", got)
	}
	got = projectDoc(d, []string{"missing"}, true)
	if got.Metadata != nil || got.Content != "" {
		t.Fatalf("project missing = %+v", got)
	}
}

func openInternalIndex(t *testing.T, dir string, opts ...Option) *Index {
	t.Helper()
	ws, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := New(ws, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := idx.Close(); err != nil {
			t.Fatal(err)
		}
	})
	return idx
}

func rootedWorkspace(t *testing.T, root string) sdkworkspace.Workspace {
	t.Helper()
	ws, err := sdkworkspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return localRootWorkspace{Workspace: ws, root: root}
}

type localRootWorkspace struct {
	sdkworkspace.Workspace
	root string
}

func (w localRootWorkspace) Root() string { return w.root }

func iterIDs(t *testing.T, idx *Index, ns, cursor string, batch int) []string {
	t.Helper()
	docs, next, err := idx.Iterate(context.Background(), ns, cursor, batch)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	if len(docs) == batch && next == "" {
		t.Fatalf("next cursor is empty for full batch")
	}
	return ids
}

func mustNotPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	fn()
}

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) (retrieval.Index, func()) {
		t.Helper()
		ws, err := sdkworkspace.NewLocalWorkspace(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		idx, err := New(ws)
		if err != nil {
			t.Fatal(err)
		}
		return idx, func() {}
	})
}

const benchVectorDims = 32

func BenchmarkOpenNamespacesAndUpsert(b *testing.B) {
	namespaceCounts := []int{1, 10, 100}
	if n := envInt("BBH_BENCH_NAMESPACES", 0); n > 0 {
		namespaceCounts = []int{n}
	}
	ctx := context.Background()
	for _, namespaces := range namespaceCounts {
		b.Run(fmt.Sprintf("namespaces=%d", namespaces), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(namespaces), "namespaces/op")
			for i := 0; i < b.N; i++ {
				idx := newBenchIndex(b, b.TempDir())
				for ns := 0; ns < namespaces; ns++ {
					doc := benchDoc(fmt.Sprintf("doc-%d", i), ns)
					if err := idx.Upsert(ctx, fmt.Sprintf("runtime-a/user-a/agent-%d", ns), []retrieval.Doc{doc}); err != nil {
						b.Fatal(err)
					}
				}
				if err := idx.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSearchText(b *testing.B) {
	docs := envInt("BBH_BENCH_DOCS", 1000)
	idx, ns := seededBenchIndex(b, docs, false)
	ctx := context.Background()
	req := retrieval.SearchRequest{QueryText: "invoice latency profile", TopK: 10}

	b.ReportAllocs()
	b.ReportMetric(float64(docs), "docs")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := idx.Search(ctx, ns, req)
		if err != nil {
			b.Fatal(err)
		}
		if len(resp.Hits) == 0 {
			b.Fatal("expected at least one hit")
		}
	}
}

func BenchmarkSearchVector(b *testing.B) {
	docs := envInt("BBH_BENCH_DOCS", 1000)
	idx, ns := seededBenchIndex(b, docs, true)
	ctx := context.Background()
	req := retrieval.SearchRequest{QueryVector: benchVector(7), TopK: 10}

	b.ReportAllocs()
	b.ReportMetric(float64(docs), "docs")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := idx.Search(ctx, ns, req)
		if err != nil {
			b.Fatal(err)
		}
		if len(resp.Hits) == 0 {
			b.Fatal("expected at least one hit")
		}
	}
}

func seededBenchIndex(b *testing.B, docs int, vectors bool) (*Index, string) {
	b.Helper()
	idx := newBenchIndex(b, b.TempDir())
	b.Cleanup(func() {
		if err := idx.Close(); err != nil {
			b.Fatal(err)
		}
	})
	ctx := context.Background()
	ns := "runtime-a/user-a/agent-a"
	batch := make([]retrieval.Doc, 0, 128)
	for i := 0; i < docs; i++ {
		d := benchDoc(fmt.Sprintf("doc-%06d", i), i)
		if !vectors {
			d.Vector = nil
		}
		batch = append(batch, d)
		if len(batch) == cap(batch) {
			if err := idx.Upsert(ctx, ns, batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := idx.Upsert(ctx, ns, batch); err != nil {
			b.Fatal(err)
		}
	}
	return idx, ns
}

func newBenchIndex(b *testing.B, dir string) *Index {
	b.Helper()
	ws, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		b.Fatal(err)
	}
	idx, err := New(ws)
	if err != nil {
		b.Fatal(err)
	}
	return idx
}

func benchDoc(id string, n int) retrieval.Doc {
	return retrieval.Doc{
		ID: id,
		Content: fmt.Sprintf(
			"invoice latency profile customer-%03d region-%02d %s",
			n%37,
			n%11,
			strings.Repeat("token ", 4+n%9),
		),
		Vector:    benchVector(n),
		Timestamp: time.Unix(1_700_000_000+int64(n), 0).UTC(),
		Metadata: map[string]any{
			"customer": fmt.Sprintf("customer-%03d", n%37),
			"region":   n % 11,
		},
	}
}

func benchVector(seed int) []float32 {
	v := make([]float32, benchVectorDims)
	for i := range v {
		v[i] = float32(((seed+1)*(i+3))%17) / 17
	}
	return v
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}
