package workspace_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	wsindex "github.com/GizClaw/flowcraft/sdkx/retrieval/workspace"
)

// makeFlushedSegments produces n flushed segments, each containing
// one doc whose ID is "doc-<segIdx>". A small per-segment
// MemtableMaxDocs threshold could also do this implicitly, but
// driving it via explicit Flush calls keeps the tests independent
// of memtable tuning.
func makeFlushedSegments(t *testing.T, idx *wsindex.Index, ns string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		d := retrieval.Doc{
			ID:       fmt.Sprintf("doc-%d", i),
			Content:  fmt.Sprintf("content for document number %d about brown fox", i),
			Vector:   []float32{float32(i), 0, 0, 0},
			Metadata: map[string]any{"idx": i},
		}
		if err := idx.Upsert(ctx, ns, []retrieval.Doc{d}); err != nil {
			t.Fatal(err)
		}
		if err := idx.Flush(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
}

// segmentDirs returns the names of segment directories on disk.
func segmentDirs(t *testing.T, ws sdkworkspace.Workspace, ns string) []string {
	t.Helper()
	entries, err := ws.List(context.Background(), ns+"/segments")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// MemWorkspace.List has a known quirk that returns
		// non-existent entries; verify presence of meta.json (the
		// segment-ready commit marker) before counting.
		ok, _ := ws.Exists(context.Background(), ns+"/segments/"+e.Name()+"/meta.json")
		if !ok {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

func TestCompact_MergesMinSegments(t *testing.T) {
	idx, ws := newIdx(t,
		wsindex.WithAutoCompact(false), // drive merges manually
		wsindex.WithCompactionMinSegments(3),
	)
	makeFlushedSegments(t, idx, "ns", 5)

	if got := len(segmentDirs(t, ws, "ns")); got != 5 {
		t.Fatalf("pre-compact segment dirs = %d, want 5", got)
	}
	if err := idx.Compact(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	// 3 oldest merged into 1 new -> 5 - 3 + 1 = 3 segments live.
	if got := len(segmentDirs(t, ws, "ns")); got != 3 {
		t.Errorf("post-compact segment dirs = %d, want 3", got)
	}
}

func TestCompact_PreservesSearchResults(t *testing.T) {
	idx, _ := newIdx(t,
		wsindex.WithAutoCompact(false),
		wsindex.WithCompactionMinSegments(3),
	)
	ctx := context.Background()
	makeFlushedSegments(t, idx, "ns", 5)

	pre, err := idx.Search(ctx, "ns", retrieval.SearchRequest{
		QueryText: "fox",
		TopK:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	preIDs := hitIDs(pre)

	if err := idx.Compact(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	post, err := idx.Search(ctx, "ns", retrieval.SearchRequest{
		QueryText: "fox",
		TopK:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	postIDs := hitIDs(post)

	if !sameSet(preIDs, postIDs) {
		t.Errorf("compaction changed result set: pre=%v post=%v", preIDs, postIDs)
	}
}

func TestCompact_TailAnchoredDropsTombstones(t *testing.T) {
	idx, ws := newIdx(t,
		wsindex.WithAutoCompact(false),
		wsindex.WithCompactionMinSegments(2),
	)
	ctx := context.Background()

	// seg 1: doc-0 + doc-1
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{
		{ID: "doc-0", Content: "alpha"}, {ID: "doc-1", Content: "beta"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	// seg 2: tombstone doc-0
	if err := idx.Delete(ctx, "ns", []string{"doc-0"}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	// Pre: 2 segments, doc-0 should be suppressed by Search
	if got := len(segmentDirs(t, ws, "ns")); got != 2 {
		t.Fatalf("pre-compact = %d, want 2", got)
	}

	if err := idx.Compact(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	// Post: 1 merged segment (tail-anchored: drops the tombstone),
	// doc-0 must not resurface.
	if got := len(segmentDirs(t, ws, "ns")); got != 1 {
		t.Errorf("post-compact = %d, want 1", got)
	}
	d, ok, err := idx.Get(ctx, "ns", "doc-0")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("doc-0 resurfaced post-merge: %+v", d)
	}
	d2, ok2, err := idx.Get(ctx, "ns", "doc-1")
	if err != nil || !ok2 {
		t.Fatalf("doc-1 should survive merge: ok=%v err=%v", ok2, err)
	}
	if d2.Content != "beta" {
		t.Errorf("doc-1 content = %q, want beta", d2.Content)
	}
}

func TestCompact_BelowThresholdIsNoop(t *testing.T) {
	idx, ws := newIdx(t,
		wsindex.WithAutoCompact(false),
		wsindex.WithCompactionMinSegments(4),
	)
	makeFlushedSegments(t, idx, "ns", 3)
	pre := len(segmentDirs(t, ws, "ns"))
	if err := idx.Compact(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	if got := len(segmentDirs(t, ws, "ns")); got != pre {
		t.Errorf("below-threshold compact changed segments: pre=%d post=%d", pre, got)
	}
}

func TestCompact_RespectsMaxSize(t *testing.T) {
	idx, ws := newIdx(t,
		wsindex.WithAutoCompact(false),
		wsindex.WithCompactionMinSegments(2),
		// MaxSize=1 byte: every segment is ineligible. Picker
		// returns no group; segment count is unchanged.
		wsindex.WithCompactionMaxSize(1),
	)
	makeFlushedSegments(t, idx, "ns", 4)
	pre := len(segmentDirs(t, ws, "ns"))
	if err := idx.Compact(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	if got := len(segmentDirs(t, ws, "ns")); got != pre {
		t.Errorf("MaxSize=1 should make every seg ineligible; pre=%d post=%d", pre, got)
	}
}

func TestCompact_AutoTriggeredByFlush(t *testing.T) {
	// Auto-compact ON with a tight wake interval; after enough
	// flushes the worker should converge segment count below the
	// threshold without an explicit Compact call.
	idx, ws := newIdx(t,
		wsindex.WithAutoCompact(true),
		wsindex.WithCompactionMinSegments(3),
		wsindex.WithCompactionInterval(20*time.Millisecond),
	)
	makeFlushedSegments(t, idx, "ns", 6)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n := len(segmentDirs(t, ws, "ns")); n < 6 {
			return // worker has run at least once
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Errorf("auto-compactor never reduced segment count below 6")
}

func TestCompact_OnUnknownNamespaceIsNoop(t *testing.T) {
	idx, _ := newIdx(t, wsindex.WithAutoCompact(false))
	if err := idx.Compact(context.Background(), "never-used"); err != nil {
		t.Errorf("Compact on never-touched namespace: %v", err)
	}
}

func TestCompact_AfterCloseReturnsErrClosed(t *testing.T) {
	idx, _ := newIdx(t, wsindex.WithAutoCompact(false))
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	err := idx.Compact(context.Background(), "ns")
	if err == nil {
		t.Fatal("expected ErrClosed, got nil")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("err = %v, want ErrClosed", err)
	}
}

// hitIDs / sameSet are tiny test helpers.
func hitIDs(r *retrieval.SearchResponse) []string {
	out := make([]string, 0, len(r.Hits))
	for _, h := range r.Hits {
		out = append(out, h.Doc.ID)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int)
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
