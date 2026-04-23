package journal

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

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
