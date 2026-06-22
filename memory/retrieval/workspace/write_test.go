package workspace_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	wsindex "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// fixedClock returns a deterministic time so segment timestamps
// across test runs are reproducible. Concurrent callers (e.g.
// background compactor + main test goroutine) are safe because the
// offset is incremented atomically.
func fixedClock(seed int64) func() time.Time {
	t := time.Unix(seed, 0).UTC()
	var off atomic.Int64
	return func() time.Time {
		n := off.Add(1)
		return t.Add(time.Duration(n) * time.Second)
	}
}

func newIdx(t *testing.T, opts ...wsindex.Option) (*wsindex.Index, sdkworkspace.Workspace) {
	t.Helper()
	ws := sdkworkspace.NewMemWorkspace()
	full := append([]wsindex.Option{wsindex.WithClock(fixedClock(1700000000))}, opts...)
	idx, err := wsindex.New(ws, full...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx, ws
}

type failingWriteWorkspace struct {
	sdkworkspace.Workspace
	enabled bool
	suffix  string
}

func (w *failingWriteWorkspace) Write(ctx context.Context, path string, data []byte) error {
	if w.enabled && strings.HasSuffix(path, w.suffix) {
		return errors.New("injected workspace write failure")
	}
	return w.Workspace.Write(ctx, path, data)
}

func TestNew_NilWorkspace(t *testing.T) {
	if _, err := wsindex.New(nil); err == nil {
		t.Fatal("expected error for nil workspace")
	} else if !errdefs.IsValidation(err) {
		t.Errorf("err = %v, want validation", err)
	}
}

func TestFlushFailureRestoresMemtableAndCanRetry(t *testing.T) {
	ctx := context.Background()
	ws := &failingWriteWorkspace{
		Workspace: sdkworkspace.NewMemWorkspace(),
		suffix:    "manifest.json.tmp",
	}
	idx, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700000000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	ws.enabled = true
	if err := idx.Flush(ctx, "ns"); err == nil {
		t.Fatal("expected injected flush failure")
	}
	resp, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("acknowledged write disappeared after failed flush: %+v", resp.Hits)
	}

	ws.enabled = false
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700001000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resp, err = reopened.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("reopened index lost retried flush doc: %+v", resp.Hits)
	}
}

func TestSegmentWriteFailureRestoresMemtableAndCanRetry(t *testing.T) {
	ctx := context.Background()
	ws := &failingWriteWorkspace{
		Workspace: sdkworkspace.NewMemWorkspace(),
		suffix:    "docs.jsonl",
	}
	idx, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700000000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	ws.enabled = true
	if err := idx.Flush(ctx, "ns"); err == nil {
		t.Fatal("expected injected segment write failure")
	}
	resp, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("acknowledged write disappeared after failed segment write: %+v", resp.Hits)
	}

	ws.enabled = false
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700001000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resp, err = reopened.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("reopened index lost retried segment doc: %+v", resp.Hits)
	}
}

func TestFlushFailureRestoresTombstoneAndCanRetry(t *testing.T) {
	ctx := context.Background()
	ws := &failingWriteWorkspace{
		Workspace: sdkworkspace.NewMemWorkspace(),
		suffix:    "manifest.json.tmp",
	}
	idx, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700000000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, "ns", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	ws.enabled = true
	if err := idx.Flush(ctx, "ns"); err == nil {
		t.Fatal("expected injected flush failure")
	}
	resp, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 0 {
		t.Fatalf("deleted doc resurfaced after failed tombstone flush: %+v", resp.Hits)
	}

	ws.enabled = false
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700001000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resp, err = reopened.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 0 {
		t.Fatalf("deleted doc resurfaced after retry/reopen: %+v", resp.Hits)
	}
}

func TestSegmentWriteFailureRestoresTombstoneAndCanRetry(t *testing.T) {
	ctx := context.Background()
	ws := &failingWriteWorkspace{
		Workspace: sdkworkspace.NewMemWorkspace(),
		suffix:    "tombstones.bin",
	}
	idx, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700000000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ws.enabled = false
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, "ns", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	ws.enabled = true
	if err := idx.Flush(ctx, "ns"); err == nil {
		t.Fatal("expected injected tombstone segment write failure")
	}
	resp, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 0 {
		t.Fatalf("deleted doc resurfaced after failed tombstone segment write: %+v", resp.Hits)
	}

	ws.enabled = false
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700001000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resp, err = reopened.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 0 {
		t.Fatalf("deleted doc resurfaced after retried tombstone segment write: %+v", resp.Hits)
	}
}

func TestFailedByteTriggeredFlushRestoresPressure(t *testing.T) {
	ctx := context.Background()
	ws := &failingWriteWorkspace{
		Workspace: sdkworkspace.NewMemWorkspace(),
		suffix:    "manifest.json.tmp",
	}
	idx, err := wsindex.New(ws,
		wsindex.WithClock(fixedClock(1700000000)),
		wsindex.WithAutoCompact(false),
		wsindex.WithMemtableMaxDocs(100),
		wsindex.WithMemtableMaxBytes(100),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ws.enabled = true
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "big", Content: strings.Repeat("alpha ", 30)}}); err == nil {
		t.Fatal("expected byte-triggered flush failure")
	}
	ws.enabled = false
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "small", Content: "bravo"}}); err != nil {
		t.Fatal(err)
	}
	if exists, _ := ws.Exists(ctx, "ns/manifest.json"); !exists {
		t.Fatal("restored byte pressure did not trigger a successful manifest publish")
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700001000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, q := range []struct {
		text string
		id   string
	}{
		{text: "alpha", id: "big"},
		{text: "bravo", id: "small"},
	} {
		resp, err := reopened.Search(ctx, "ns", retrieval.SearchRequest{QueryText: q.text, TopK: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != q.id {
			t.Fatalf("reopened hits for %q = %+v, want %s", q.text, resp.Hits, q.id)
		}
	}
}

func TestUpsertDeepCopiesStagedDoc(t *testing.T) {
	idx, _ := newIdx(t, wsindex.WithAutoCompact(false))
	ctx := context.Background()
	nestedTags := []any{"before"}
	nestedMap := map[string]any{"tags": nestedTags}
	typedMap := map[string][]string{"labels": {"before"}}
	typedSlice := []map[string]string{{"state": "before"}}
	structValue := struct {
		Tags   []string
		Nested map[string]string
	}{
		Tags:   []string{"before"},
		Nested: map[string]string{"state": "before"},
	}
	docs := []retrieval.Doc{{
		ID:           "a",
		Content:      "alpha",
		Vector:       []float32{1, 0},
		Metadata:     map[string]any{"state": "before", "nested": nestedMap, "typed_map": typedMap, "typed_slice": typedSlice, "struct": structValue},
		SparseVector: map[string]float32{"alpha": 1},
	}}
	if err := idx.Upsert(ctx, "ns", docs); err != nil {
		t.Fatal(err)
	}
	docs[0].Vector[0] = 0
	docs[0].Vector[1] = 1
	docs[0].Metadata["state"] = "after"
	docs[0].SparseVector["alpha"] = 99
	nestedTags[0] = "after"
	nestedMap["new"] = "after"
	typedMap["labels"][0] = "after"
	typedMap["labels"] = append(typedMap["labels"], "new")
	typedSlice[0]["state"] = "after"
	structValue.Tags[0] = "after"
	structValue.Nested["state"] = "after"

	resp, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: 1, WithVector: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%+v", resp.Items)
	}
	got := resp.Items[0]
	if got.Vector[0] != 1 || got.Metadata["state"] != "before" || got.SparseVector["alpha"] != 1 {
		t.Fatalf("staged doc aliases caller-owned data: %+v", got)
	}
	gotNested, ok := got.Metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested metadata has unexpected type: %+v", got.Metadata["nested"])
	}
	gotTags, ok := gotNested["tags"].([]any)
	if !ok || len(gotTags) != 1 || gotTags[0] != "before" {
		t.Fatalf("nested metadata slice aliases caller-owned data: %+v", got.Metadata)
	}
	if _, ok := gotNested["new"]; ok {
		t.Fatalf("nested metadata map aliases caller-owned data: %+v", got.Metadata)
	}
	gotTypedMap, ok := got.Metadata["typed_map"].(map[string][]string)
	if !ok || len(gotTypedMap["labels"]) != 1 || gotTypedMap["labels"][0] != "before" {
		t.Fatalf("typed nested metadata map aliases caller-owned data: %+v", got.Metadata)
	}
	gotTypedSlice, ok := got.Metadata["typed_slice"].([]map[string]string)
	if !ok || len(gotTypedSlice) != 1 || gotTypedSlice[0]["state"] != "before" {
		t.Fatalf("typed nested metadata slice aliases caller-owned data: %+v", got.Metadata)
	}
	gotStruct, ok := got.Metadata["struct"].(struct {
		Tags   []string
		Nested map[string]string
	})
	if !ok || len(gotStruct.Tags) != 1 || gotStruct.Tags[0] != "before" || gotStruct.Nested["state"] != "before" {
		t.Fatalf("struct metadata aliases caller-owned data: %+v", got.Metadata)
	}
}

func TestUpsertRejectsCyclicMetadata(t *testing.T) {
	idx, _ := newIdx(t, wsindex.WithAutoCompact(false))
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{
		ID:       "cyclic",
		Content:  "alpha",
		Metadata: map[string]any{"cyclic": cyclic},
	}})
	if err == nil {
		t.Fatal("expected cyclic metadata marshal error")
	}
}

func TestUpsert_PersistsViaWAL(t *testing.T) {
	idx, ws := newIdx(t)

	docs := []retrieval.Doc{
		{ID: "a", Content: "hello world"},
		{ID: "b", Content: "second doc"},
	}
	if err := idx.Upsert(context.Background(), "ns1", docs); err != nil {
		t.Fatal(err)
	}

	// WAL must contain entries before any flush happens.
	entries, err := ws.List(context.Background(), "ns1/wal")
	if err != nil {
		t.Fatalf("wal dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected wal entries, got 0")
	}
}

func TestUpsert_EmptyIDRejected(t *testing.T) {
	idx, _ := newIdx(t)
	err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{Content: "x"}})
	if err == nil || !strings.Contains(err.Error(), "doc.ID") {
		t.Errorf("expected doc.ID empty error, got %v", err)
	}
}

func TestFlush_CreatesSegmentAndAdvancesManifest(t *testing.T) {
	idx, ws := newIdx(t)
	docs := []retrieval.Doc{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if err := idx.Upsert(context.Background(), "ns", docs); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := ws.Exists(context.Background(), "ns/manifest.json"); !exists {
		t.Fatal("manifest.json missing after Flush")
	}
	// First segment ID is always 1 (LastSegmentID starts at 0).
	// Direct Exists is preferred over counting via List because
	// MemWorkspace.List has an existing quirk that surfaces parent
	// dir basenames when the listed path itself is registered as a
	// dir — out of scope for this PR; a separate sdk fix tracks it.
	if exists, _ := ws.Exists(context.Background(),
		"ns/segments/0000000000000001"); !exists {
		t.Fatal("expected segment 1 directory")
	}
}

func TestFlush_SegmentMetaIsCommitMarker(t *testing.T) {
	idx, ws := newIdx(t)
	if err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{ID: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	// meta.json is the segment-ready commit marker; verify the
	// concrete path exists rather than walking via List.
	if exists, _ := ws.Exists(context.Background(),
		"ns/segments/0000000000000001/meta.json"); !exists {
		t.Error("segment 1 missing meta.json commit marker")
	}
}

func TestReopen_ReplaysWALIntoMemtable(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	clk := fixedClock(1700000000)

	idx, err := wsindex.New(ws, wsindex.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{
		{ID: "a"},
		{ID: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	// Close WITHOUT flushing -> records still in WAL only.
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and immediately flush; the resulting segment must
	// contain the docs that were only in the WAL on close.
	idx2, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700001000)))
	if err != nil {
		t.Fatal(err)
	}
	if err := idx2.Flush(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx2.Close() })

	// Segment must be present after reopen+flush. The replayed
	// memtable carried "a" and "b", so segment 1 should exist.
	if exists, _ := ws.Exists(context.Background(),
		"ns/segments/0000000000000001/meta.json"); !exists {
		t.Fatal("no segment after reopen+flush — WAL replay failed")
	}
}

func TestFlush_ThresholdTriggersAuto(t *testing.T) {
	idx, ws := newIdx(t, wsindex.WithMemtableMaxDocs(3))
	for _, id := range []string{"a", "b", "c"} { // hits threshold
		if err := idx.Upsert(context.Background(), "ns",
			[]retrieval.Doc{{ID: id}}); err != nil {
			t.Fatal(err)
		}
	}
	if exists, _ := ws.Exists(context.Background(),
		"ns/segments/0000000000000001/meta.json"); !exists {
		t.Error("auto-flush did not produce a segment")
	}
}

func TestDelete_AppendsTombstoneToWAL(t *testing.T) {
	idx, ws := newIdx(t)
	if err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(context.Background(), "ns", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := ws.List(context.Background(), "ns/wal")
	if len(entries) == 0 {
		t.Fatal("expected wal entries after delete")
	}
}

func TestClose_IsIdempotent(t *testing.T) {
	idx, _ := newIdx(t)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("second Close should be no-op, got %v", err)
	}
}

func TestUpsert_AfterCloseReturnsErrClosed(t *testing.T) {
	idx, _ := newIdx(t)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{ID: "a"}})
	if !errors.Is(err, wsindex.ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("ErrClosed should be NotAvailable category, err=%v", err)
	}
}

func TestEmptyNamespaceRejected(t *testing.T) {
	idx, _ := newIdx(t)
	err := idx.Upsert(context.Background(), "", []retrieval.Doc{{ID: "a"}})
	if err == nil || !errdefs.IsValidation(err) {
		t.Errorf("expected validation error, got %v", err)
	}
}

func TestFlush_AdvancesFirstUnconsumedWALSeq(t *testing.T) {
	idx, ws := newIdx(t)
	// Two flush rounds: the second must observe a higher
	// FirstUnconsumedWALSeq, evidence that consumed WAL files are
	// being retired.
	if err := idx.Upsert(context.Background(), "ns",
		[]retrieval.Doc{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(context.Background(), "ns",
		[]retrieval.Doc{{ID: "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	// After two flushes, both early WAL logs should be gone.
	entries, _ := ws.List(context.Background(), "ns/wal")
	stale := 0
	for _, e := range entries {
		// Only the most recent (current writer) log should remain.
		if !e.IsDir() {
			stale++
		}
	}
	if stale > 1 {
		t.Errorf("expected at most 1 wal file after two flushes, got %d", stale)
	}
}
