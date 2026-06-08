package workspace_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	wsindex "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestDrop_RemovesNamespaceAndAllowsRecreate(t *testing.T) {
	idx, ws := newIdx(t, wsindex.WithAutoCompact(false))
	ctx := context.Background()
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "bravo"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := ws.Exists(ctx, "ns/manifest.json"); !exists {
		t.Fatal("manifest missing before Drop")
	}

	if err := idx.Drop(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := ws.Exists(ctx, "ns/manifest.json"); exists {
		t.Fatal("manifest still exists after Drop")
	}
	search, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Hits) != 0 {
		t.Fatalf("Search after Drop returned hits: %+v", search.Hits)
	}
	list, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 || list.Total != 0 {
		t.Fatalf("List after Drop = %+v, want empty", list)
	}
	if got, ok, err := idx.Get(ctx, "ns", "a"); err != nil || ok {
		t.Fatalf("Get after Drop got=%+v ok=%v err=%v, want miss", got, ok, err)
	}
	n, err := idx.Count(ctx, "ns", retrieval.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Count after Drop = %d, want 0", n)
	}

	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "fresh", Content: "charlie"}}); err != nil {
		t.Fatal(err)
	}
	search, err = idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "charlie", TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Hits) != 1 || search.Hits[0].Doc.ID != "fresh" {
		t.Fatalf("recreated namespace search = %+v, want fresh", search.Hits)
	}
}

func TestDrop_RemovesPendingMemtableAndAllowsFreshReopen(t *testing.T) {
	idx, _ := newIdx(t, wsindex.WithAutoCompact(false))
	ctx := context.Background()
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{
		{ID: "a", Content: "alpha pending"},
		{ID: "b", Content: "bravo pending"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := idx.Drop(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	search, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "alpha", TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Hits) != 0 {
		t.Fatalf("Search after Drop returned pending hits: %+v", search.Hits)
	}
	list, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 || list.Total != 0 {
		t.Fatalf("List after Drop = %+v, want empty", list)
	}
	if got, ok, err := idx.Get(ctx, "ns", "a"); err != nil || ok {
		t.Fatalf("Get after Drop got=%+v ok=%v err=%v, want miss", got, ok, err)
	}
	n, err := idx.Count(ctx, "ns", retrieval.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Count after Drop = %d, want 0", n)
	}

	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "fresh", Content: "fresh pending"}}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := idx.Get(ctx, "ns", "fresh")
	if err != nil || !ok || got.Content != "fresh pending" {
		t.Fatalf("Get after recreate got=%+v ok=%v err=%v, want fresh", got, ok, err)
	}
}

func TestDrop_AfterCompactRemovesNamespaceRoot(t *testing.T) {
	idx, ws := newIdx(t,
		wsindex.WithAutoCompact(false),
		wsindex.WithCompactionMinSegments(2),
	)
	ctx := context.Background()
	makeFlushedSegments(t, idx, "ns", 3)
	if err := idx.Compact(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if exists, err := ws.Exists(ctx, "ns"); err != nil || !exists {
		t.Fatalf("namespace root before Drop exists=%v err=%v, want true", exists, err)
	}

	if err := idx.Drop(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if exists, err := ws.Exists(ctx, "ns"); err != nil || exists {
		t.Fatalf("namespace root after Drop exists=%v err=%v, want false", exists, err)
	}

	reopened, err := wsindex.New(ws, wsindex.WithClock(fixedClock(1700005000)), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	list, err := reopened.List(ctx, "ns", retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 || list.Total != 0 {
		t.Fatalf("fresh reopen after Drop = %+v, want empty", list)
	}
}

func TestDrop_WithLiveLockReleasesNamespaceAndCloseIsSafe(t *testing.T) {
	idx, ws := newIdx(t,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(time.Second),
	)
	ctx := context.Background()
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	if exists, err := ws.Exists(ctx, "ns/.lock"); err != nil || !exists {
		t.Fatalf("lock before Drop exists=%v err=%v, want true", exists, err)
	}

	if err := idx.Drop(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if exists, err := ws.Exists(ctx, "ns"); err != nil || exists {
		t.Fatalf("namespace after Drop exists=%v err=%v, want false", exists, err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close after Drop: %v", err)
	}
}

func TestDrop_NonExistentIsNoopAndEmptyNamespaceRejected(t *testing.T) {
	idx, ws := newIdx(t, wsindex.WithAutoCompact(false))
	ctx := context.Background()
	if err := idx.Drop(ctx, "missing"); err != nil {
		t.Fatalf("Drop missing namespace: %v", err)
	}
	if exists, err := ws.Exists(ctx, "missing"); err != nil || exists {
		t.Fatalf("Drop missing namespace created root exists=%v err=%v, want false", exists, err)
	}
	err := idx.Drop(ctx, "")
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Drop empty namespace err=%v, want validation", err)
	}
}

func TestDrop_AfterCloseReturnsErrClosed(t *testing.T) {
	idx, _ := newIdx(t, wsindex.WithAutoCompact(false))
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Drop(context.Background(), "ns"); !errors.Is(err, wsindex.ErrClosed) {
		t.Fatalf("Drop after Close err=%v, want ErrClosed", err)
	}
}
