package knowledge

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestFSStore_AddGetDelete(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	err := store.AddDocument(ctx, "ds1", "doc1.md", "# Hello\nThis is a test document.")
	if err != nil {
		t.Fatal(err)
	}

	doc, err := store.GetDocument(ctx, "ds1", "doc1.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != "doc1.md" {
		t.Fatalf("expected doc1.md, got %q", doc.Name)
	}
	if doc.Content != "# Hello\nThis is a test document." {
		t.Fatalf("unexpected content: %q", doc.Content)
	}

	if err := store.DeleteDocument(ctx, "ds1", "doc1.md"); err != nil {
		t.Fatal(err)
	}

	_, err = store.GetDocument(ctx, "ds1", "doc1.md")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestFSStore_AddWithFrontmatter(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	content := "---\ntitle: Test Doc\n---\n# Content\nBody here"
	err := store.AddDocument(ctx, "ds1", "fm.md", content)
	if err != nil {
		t.Fatal(err)
	}

	doc, err := store.GetDocument(ctx, "ds1", "fm.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Metadata["title"] != "Test Doc" {
		t.Fatalf("expected metadata title, got %v", doc.Metadata)
	}
	if doc.Content != "# Content\nBody here" {
		t.Fatalf("unexpected content: %q", doc.Content)
	}
}

func TestFSStore_ListDocuments(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "a.md", "doc a")
	_ = store.AddDocument(ctx, "ds1", "b.md", "doc b")

	docs, err := store.ListDocuments(ctx, "ds1")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
}

func TestFSStore_SearchL2(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 100, ChunkOverlap: 10}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go is a statically typed compiled programming language designed at Google")
	_ = store.AddDocument(ctx, "ds1", "python.md", "Python is a high-level general-purpose programming language")

	results, err := store.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results")
	}
	if results[0].Layer != LayerDetail {
		t.Fatalf("expected L2 layer, got %s", results[0].Layer)
	}
}

func TestFSStore_SearchL0(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "doc.md", "Go programming language")
	// Manually set abstract
	store.SetDocAbstract("ds1", "doc.md", "Go is a compiled language for system programming")

	results, err := store.Search(ctx, "ds1", "Go programming", SearchOptions{MaxLayer: LayerAbstract, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results with L0 layer")
	}
	for _, r := range results {
		if r.Layer != LayerAbstract {
			t.Fatalf("expected L0, got %s", r.Layer)
		}
	}
}

func TestFSStore_AbstractOverview(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "doc.md", "content")
	store.SetDocAbstract("ds1", "doc.md", "abstract text")
	store.SetDocOverview("ds1", "doc.md", "overview text")

	abs, _ := store.Abstract(ctx, "ds1", "doc.md")
	if abs != "abstract text" {
		t.Fatalf("expected abstract, got %q", abs)
	}

	ov, _ := store.Overview(ctx, "ds1", "doc.md")
	if ov != "overview text" {
		t.Fatalf("expected overview, got %q", ov)
	}
}

func TestFSStore_DatasetAbstractOverview(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "doc.md", "content")
	store.SetDatasetAbstract("ds1", "dataset abstract")
	store.SetDatasetOverview("ds1", "dataset overview")

	abs, _ := store.DatasetAbstract(ctx, "ds1")
	if abs != "dataset abstract" {
		t.Fatalf("expected dataset abstract, got %q", abs)
	}

	ov, _ := store.DatasetOverview(ctx, "ds1")
	if ov != "dataset overview" {
		t.Fatalf("expected dataset overview, got %q", ov)
	}
}

func TestFSStore_EmptyRequired(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	err := store.AddDocument(ctx, "", "doc.md", "content")
	if err == nil {
		t.Fatal("expected error for empty dataset_id")
	}

	err = store.AddDocument(ctx, "ds", "", "content")
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestFSStore_WithTokenizer(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithTokenizer(&CJKTokenizer{}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "doc.md", "知识检索世界 Go programming")
	results, err := store.Search(ctx, "ds1", "知识检索", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected CJK search results")
	}
}

func TestFSStore_AbstractStats(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "doc.md", "Go programming language reference guide")
	store.SetDocAbstract("ds1", "doc.md", "Go is a compiled language for system programming")

	results, err := store.Search(ctx, "ds1", "Go programming", SearchOptions{MaxLayer: LayerAbstract, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected L0 search results using abstractStats")
	}

	store.SetDocAbstract("ds1", "doc.md", "Updated abstract about Go compilation")
	results2, err := store.Search(ctx, "ds1", "compilation", SearchOptions{MaxLayer: LayerAbstract, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results2) == 0 {
		t.Fatal("expected results after abstract update")
	}
}

func TestFSStore_DefaultThreshold(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go is a statically typed compiled programming language designed at Google")
	_ = store.AddDocument(ctx, "ds1", "random.md", "The weather today is sunny with clear skies and mild temperatures")

	results, err := store.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Score < DefaultThreshold {
			t.Fatalf("result score %f is below default threshold %f", r.Score, DefaultThreshold)
		}
	}
}

func TestFSStore_InvertedIndex_BasicSearch(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go is a statically typed compiled programming language")
	_ = store.AddDocument(ctx, "ds1", "python.md", "Python is a high-level interpreted scripting language")
	_ = store.AddDocument(ctx, "ds1", "rust.md", "Rust is a systems programming language focused on safety")

	results, err := store.Search(ctx, "ds1", "programming language", SearchOptions{TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results via inverted index")
	}
}

func TestFSStore_InvertedIndex_AfterDelete(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go programming language")
	_ = store.AddDocument(ctx, "ds1", "py.md", "Python scripting language")

	_ = store.DeleteDocument(ctx, "ds1", "go.md")

	results, err := store.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.DocName == "go.md" {
			t.Fatal("deleted document should not appear in results")
		}
	}
}

func TestFSStore_InvertedIndex_UpdateDocument(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "doc.md", "Original content about Go programming")
	_ = store.AddDocument(ctx, "ds1", "doc.md", "Updated content about Rust systems programming")

	results, err := store.Search(ctx, "ds1", "Rust systems", SearchOptions{TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for updated content")
	}
}

func TestIsMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"test.md", true},
		{"test.txt", true},
		{"test.markdown", true},
		{"test.go", false},
		{"test.py", false},
	}
	for _, tt := range tests {
		if isMarkdown(tt.name) != tt.expected {
			t.Errorf("isMarkdown(%q) = %v, want %v", tt.name, !tt.expected, tt.expected)
		}
	}
}

func TestFSStore_AddDocuments_Batch(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	docs := []DocInput{
		{Name: "go.md", Content: "Go programming language concurrency"},
		{Name: "py.md", Content: "Python scripting language dynamic typing"},
		{Name: "rust.md", Content: "Rust systems programming safety performance"},
	}
	if err := store.AddDocuments(ctx, "ds1", docs); err != nil {
		t.Fatal(err)
	}

	listed, err := store.ListDocuments(ctx, "ds1")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("expected 3 docs, got %d", len(listed))
	}

	results, err := store.Search(ctx, "ds1", "Go concurrency", SearchOptions{TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results after batch add")
	}
	if results[0].DocName != "go.md" {
		t.Errorf("expected top result go.md, got %s", results[0].DocName)
	}
}

func TestFSStore_AddDocuments_Empty(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	if err := store.AddDocuments(context.Background(), "ds1", nil); err != nil {
		t.Fatal(err)
	}
}

func TestFSStore_AddDocuments_Upsert(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "doc.md", "original content")

	err := store.AddDocuments(ctx, "ds1", []DocInput{{Name: "doc.md", Content: "updated content"}})
	if err != nil {
		t.Fatal(err)
	}

	doc, _ := store.GetDocument(ctx, "ds1", "doc.md")
	if doc.Content != "updated content" {
		t.Fatalf("expected updated content, got %q", doc.Content)
	}
}

func TestFSStore_ReindexVectors(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	embedder := &mockEmbedder{dim: 3}
	store := NewFSStore(ws, WithEmbedder(embedder))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go programming")
	_ = store.AddDocument(ctx, "ds1", "py.md", "Python scripting")

	// Verify vectors exist after AddDocument
	r1, _ := store.Search(ctx, "ds1", "Go", SearchOptions{TopK: 2, Mode: ModeHybrid})
	if len(r1) == 0 {
		t.Fatal("expected hybrid results before rebuild")
	}

	// Simulate BuildIndex (which clears vectors)
	_ = store.BuildIndex(ctx)

	// Vectors should be empty after BuildIndex
	store.mu.RLock()
	di := store.index["ds1"]
	vecCount := len(di.vectors)
	store.mu.RUnlock()
	if vecCount != 0 {
		t.Fatalf("expected 0 vectors after BuildIndex, got %d", vecCount)
	}

	// ReindexVectors should restore them
	if err := store.ReindexVectors(ctx); err != nil {
		t.Fatal(err)
	}

	store.mu.RLock()
	di = store.index["ds1"]
	vecCount = len(di.vectors)
	store.mu.RUnlock()
	if vecCount == 0 {
		t.Fatal("expected vectors after ReindexVectors")
	}

	r2, _ := store.Search(ctx, "ds1", "Go", SearchOptions{TopK: 2, Mode: ModeHybrid})
	if len(r2) == 0 {
		t.Fatal("expected hybrid results after ReindexVectors")
	}
}

func TestFSStore_ReindexVectors_NoEmbedder(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	if err := store.ReindexVectors(context.Background()); err != nil {
		t.Fatalf("expected nil error without embedder, got %v", err)
	}
}
