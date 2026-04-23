package knowledge

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func newTestRetrievalStore(t *testing.T) *RetrievalStore {
	t.Helper()
	idx := memory.New()
	return NewRetrievalStore(idx, WithRetrievalChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
}

func TestRetrievalStore_AddAndSearch(t *testing.T) {
	ctx := context.Background()
	s := newTestRetrievalStore(t)
	if err := s.AddDocument(ctx, "ds1", "go.md", "Go is a programming language built at Google. It excels at concurrency."); err != nil {
		t.Fatalf("add: %v", err)
	}
	res, err := s.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("expected at least one result")
	}
	if res[0].DocName != "go.md" {
		t.Fatalf("unexpected doc name %q", res[0].DocName)
	}
}

func TestRetrievalStore_GetAndList(t *testing.T) {
	ctx := context.Background()
	s := newTestRetrievalStore(t)
	if err := s.AddDocuments(ctx, "ds1", []DocInput{
		{Name: "a.md", Content: "Apple."},
		{Name: "b.md", Content: "Banana."},
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	docs, err := s.ListDocuments(ctx, "ds1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	d, err := s.GetDocument(ctx, "ds1", "a.md")
	if err != nil || d == nil {
		t.Fatalf("get: %v %v", err, d)
	}
	if d.Content == "" {
		t.Fatalf("empty content")
	}
}

func TestRetrievalStore_Delete(t *testing.T) {
	ctx := context.Background()
	s := newTestRetrievalStore(t)
	_ = s.AddDocument(ctx, "ds1", "x.md", "hello world")
	if err := s.DeleteDocument(ctx, "ds1", "x.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	d, _ := s.GetDocument(ctx, "ds1", "x.md")
	if d != nil {
		t.Fatalf("expected nil after delete")
	}
}

func TestRetrievalStore_AbstractAndOverview(t *testing.T) {
	ctx := context.Background()
	s := newTestRetrievalStore(t)
	_ = s.AddDocument(ctx, "ds1", "doc.md", "raw body")
	if err := s.SetAbstract(ctx, "ds1", "doc.md", "tldr"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOverview(ctx, "ds1", "doc.md", "long overview"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Abstract(ctx, "ds1", "doc.md"); a != "tldr" {
		t.Fatalf("abstract = %q", a)
	}
	if o, _ := s.Overview(ctx, "ds1", "doc.md"); o != "long overview" {
		t.Fatalf("overview = %q", o)
	}
	if err := s.SetDatasetAbstract(ctx, "ds1", "ds-tldr"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.DatasetAbstract(ctx, "ds1"); a != "ds-tldr" {
		t.Fatalf("dataset abstract = %q", a)
	}
}
