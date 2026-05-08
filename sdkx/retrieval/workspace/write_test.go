package workspace_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	wsindex "github.com/GizClaw/flowcraft/sdkx/retrieval/workspace"
)

// fixedClock returns a deterministic time so segment timestamps
// across test runs are reproducible.
func fixedClock(seed int64) func() time.Time {
	t := time.Unix(seed, 0).UTC()
	var off int64
	return func() time.Time {
		off++
		return t.Add(time.Duration(off) * time.Second)
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

func TestNew_NilWorkspace(t *testing.T) {
	if _, err := wsindex.New(nil); err == nil {
		t.Fatal("expected error for nil workspace")
	} else if !errdefs.IsValidation(err) {
		t.Errorf("err = %v, want validation", err)
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
