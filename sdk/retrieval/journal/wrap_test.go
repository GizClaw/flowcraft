package journal

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestWrapPreservesOptionalInterfaces(t *testing.T) {
	idx := Wrap(memory.New(), NewMemoryJournal())
	if _, ok := idx.(retrieval.DocGetter); !ok {
		t.Fatal("DocGetter should be exposed (memory.Index implements it)")
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
	inner := memory.New()
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

func TestWrapDropRecordsAuditEvents(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
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

func TestWrapJournalUpsertBefore(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
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
