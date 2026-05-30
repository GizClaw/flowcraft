package namespace

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	memidx "github.com/GizClaw/flowcraft/memory/retrieval/memory"
)

func TestCopyNamespaceCopiesDocsAndLeavesSource(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	if err := idx.Upsert(ctx, "old", []retrieval.Doc{
		{ID: "a", Content: "alpha", Vector: []float32{1, 0}},
		{ID: "b", Content: "bravo", Metadata: map[string]any{"k": "v"}},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := CopyNamespace(ctx, idx, "old", "new", CopyOptions{BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Copied != 2 {
		t.Fatalf("Copied = %d, want 2", res.Copied)
	}
	for _, ns := range []string{"old", "new"} {
		page, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 10, WithVector: true})
		if err != nil {
			t.Fatalf("List(%s): %v", ns, err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("List(%s) returned %d docs, want 2", ns, len(page.Items))
		}
	}
}
