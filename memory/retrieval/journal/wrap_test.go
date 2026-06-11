package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	wsindex "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func newTestIndex(t *testing.T) *wsindex.Index {
	t.Helper()
	idx, err := wsindex.New(sdkworkspace.NewMemWorkspace(), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestWrapPreservesOptionalInterfaces(t *testing.T) {
	idx := Wrap(newTestIndex(t), NewMemoryJournal())
	if _, ok := idx.(retrieval.DocGetter); !ok {
		t.Fatal("DocGetter should be exposed")
	}
	if _, ok := idx.(retrieval.DeletableByFilter); !ok {
		t.Fatal("DeletableByFilter should be exposed")
	}
	if _, ok := idx.(retrieval.Droppable); !ok {
		t.Fatal("Droppable should be exposed")
	}
	if _, ok := idx.(retrieval.Iterable); !ok {
		t.Fatal("Iterable should be exposed")
	}
}

func TestWrapDeleteByFilterRecordsAuditEvents(t *testing.T) {
	ctx := context.Background()
	inner := newTestIndex(t)
	j := NewMemoryJournal()
	idx := Wrap(inner, j)
	ns := "ns-bulk"
	docs := []retrieval.Doc{
		{ID: "x1", Content: "foo", Metadata: map[string]any{"keep": false}, Timestamp: time.Now()},
		{ID: "x2", Content: "foo", Metadata: map[string]any{"keep": false}, Timestamp: time.Now()},
		{ID: "x3", Content: "foo", Metadata: map[string]any{"keep": true}, Timestamp: time.Now()},
	}
	if err := idx.Upsert(ctx, ns, docs); err != nil {
		t.Fatal(err)
	}
	df := idx.(retrieval.DeletableByFilter)
	n, err := df.DeleteByFilter(ctx, ns, retrieval.Filter{Eq: map[string]any{"keep": false}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deletions, got %d", n)
	}
	for _, id := range []string{"x1", "x2"} {
		h, err := j.History(ctx, ns, id)
		if err != nil {
			t.Fatal(err)
		}
		var sawDelete bool
		for _, ev := range h {
			if ev.Op == OpDelete {
				sawDelete = true
				if ev.Before == nil || ev.Before.ID != id {
					t.Fatalf("delete event missing Before for %s: %+v", id, ev)
				}
			}
		}
		if !sawDelete {
			t.Fatalf("expected OpDelete in history for %s, got %+v", id, h)
		}
	}
	h3, _ := j.History(ctx, ns, "x3")
	for _, ev := range h3 {
		if ev.Op == OpDelete {
			t.Fatalf("x3 should not have delete event, got %+v", ev)
		}
	}
}

func TestWrapDeleteByFilterDeletesSnapshottedIDs(t *testing.T) {
	ctx := context.Background()
	ns := "ns-racy-delete"
	inner := &mutatingFilterIndex{docs: map[string]retrieval.Doc{
		"x1": {ID: "x1", Content: "foo", Metadata: map[string]any{"keep": false}, Timestamp: time.Now()},
	}}
	j := NewMemoryJournal()
	idx := Wrap(inner, j).(retrieval.DeletableByFilter)

	n, err := idx.DeleteByFilter(ctx, ns, retrieval.Filter{Eq: map[string]any{"keep": false}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted=%d want 1", n)
	}
	if inner.deleteByFilterCalled {
		t.Fatal("wrapper should delete snapshotted IDs directly, not re-run DeleteByFilter")
	}
	if len(inner.deleted) != 1 || inner.deleted[0] != "x1" {
		t.Fatalf("deleted IDs = %v, want [x1]", inner.deleted)
	}
	if _, ok := inner.docs["x2"]; !ok {
		t.Fatal("x2 was added after snapshot and should not be deleted")
	}
	h, err := j.History(ctx, ns, "x1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].Op != OpDelete || h[0].Before == nil || h[0].Before.ID != "x1" {
		t.Fatalf("history for x1 = %+v", h)
	}
	if h2, _ := j.History(ctx, ns, "x2"); len(h2) != 0 {
		t.Fatalf("x2 should not have journal events, got %+v", h2)
	}
}

func TestWrapDeleteByFilterRejectsEmptyFilter(t *testing.T) {
	ctx := context.Background()
	inner := newTestIndex(t)
	j := NewMemoryJournal()
	idx := Wrap(inner, j)
	ns := "ns-empty-delete"
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{{ID: "x", Content: "foo", Timestamp: time.Now()}}); err != nil {
		t.Fatal(err)
	}

	df := idx.(retrieval.DeletableByFilter)
	n, err := df.DeleteByFilter(ctx, ns, retrieval.Filter{})
	if !errors.Is(err, retrieval.ErrEmptyDeleteFilter) {
		t.Fatalf("err=%v, want ErrEmptyDeleteFilter", err)
	}
	if n != 0 {
		t.Fatalf("deleted=%d want 0", n)
	}
	if _, found, err := idx.(retrieval.DocGetter).Get(ctx, ns, "x"); err != nil || !found {
		t.Fatalf("doc should survive empty DeleteByFilter: found=%v err=%v", found, err)
	}
	h, err := j.History(ctx, ns, "x")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range h {
		if ev.Op == OpDelete {
			t.Fatalf("empty DeleteByFilter should not record delete event: %+v", h)
		}
	}
}

func TestWrapDropRecordsAuditEvents(t *testing.T) {
	ctx := context.Background()
	inner := newTestIndex(t)
	j := NewMemoryJournal()
	idx := Wrap(inner, j)
	ns := "ns-drop"
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "a", Content: "x", Timestamp: time.Now()},
		{ID: "b", Content: "y", Timestamp: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	d := idx.(retrieval.Droppable)
	if err := d.Drop(ctx, ns); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		h, _ := j.History(ctx, ns, id)
		seen := false
		for _, ev := range h {
			if ev.Op == OpDelete {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("expected delete event for %s after Drop, got %+v", id, h)
		}
	}
}

type mutatingFilterIndex struct {
	docs                 map[string]retrieval.Doc
	deleted              []string
	iterated             bool
	deleteByFilterCalled bool
}

func (m *mutatingFilterIndex) Upsert(_ context.Context, _ string, docs []retrieval.Doc) error {
	if m.docs == nil {
		m.docs = make(map[string]retrieval.Doc)
	}
	for _, d := range docs {
		m.docs[d.ID] = d
	}
	return nil
}

func (m *mutatingFilterIndex) Delete(_ context.Context, _ string, ids []string) error {
	for _, id := range ids {
		m.deleted = append(m.deleted, id)
		delete(m.docs, id)
	}
	return nil
}

func (m *mutatingFilterIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{}, nil
}

func (m *mutatingFilterIndex) List(_ context.Context, _ string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	var out []retrieval.Doc
	for _, d := range m.docs {
		if retrieval.DocMatchesFilter(d, req.Filter) {
			out = append(out, d)
		}
	}
	return &retrieval.ListResponse{Items: out, Total: int64(len(out))}, nil
}

func (m *mutatingFilterIndex) Capabilities() retrieval.Capabilities {
	return retrieval.DefaultMemoryCapabilities()
}

func (m *mutatingFilterIndex) Close() error { return nil }

func (m *mutatingFilterIndex) Get(_ context.Context, _, id string) (retrieval.Doc, bool, error) {
	d, ok := m.docs[id]
	return d, ok, nil
}

func (m *mutatingFilterIndex) SupportsFilter(retrieval.Filter) bool { return true }

func (m *mutatingFilterIndex) DeleteByFilter(_ context.Context, _ string, f retrieval.Filter) (int64, error) {
	m.deleteByFilterCalled = true
	var ids []string
	for id, d := range m.docs {
		if retrieval.DocMatchesFilter(d, f) {
			ids = append(ids, id)
		}
	}
	_ = m.Delete(context.Background(), "", ids)
	return int64(len(ids)), nil
}

func (m *mutatingFilterIndex) Iterate(_ context.Context, _ string, cursor string, _ int) ([]retrieval.Doc, string, error) {
	if cursor != "" {
		return nil, "", nil
	}
	var out []retrieval.Doc
	for _, d := range m.docs {
		out = append(out, d)
	}
	if !m.iterated {
		m.iterated = true
		d := m.docs["x1"]
		d.Metadata = map[string]any{"keep": true}
		m.docs["x1"] = d
		m.docs["x2"] = retrieval.Doc{ID: "x2", Content: "bar", Metadata: map[string]any{"keep": false}, Timestamp: time.Now()}
	}
	return out, "", nil
}

func TestWrapJournalUpsertBefore(t *testing.T) {
	ctx := context.Background()
	inner := newTestIndex(t)
	j := NewMemoryJournal()
	idx := Wrap(inner, j)
	ns := "ns1"
	d1 := retrieval.Doc{ID: "x1", Content: "hello", Timestamp: time.Now()}
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{d1}); err != nil {
		t.Fatal(err)
	}
	h, err := j.History(ctx, ns, "x1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].Before != nil || h[0].After == nil || h[0].After.Content != "hello" {
		t.Fatalf("history=%+v", h)
	}
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{{ID: "x1", Content: "world", Timestamp: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	h2, err := j.History(ctx, ns, "x1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h2) != 2 || h2[1].Before == nil || h2[1].Before.Content != "hello" {
		t.Fatalf("second history=%+v", h2)
	}
}
