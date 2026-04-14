package knowledge

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestSearchTool_Execute(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go is a compiled programming language")

	st := NewSearchTool(store)
	if st.Definition().Name != "knowledge_search" {
		t.Fatalf("expected tool name knowledge_search, got %s", st.Definition().Name)
	}

	result, err := st.Execute(ctx, `{"query": "Go programming"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result == "" || result == "[]" || result == "null" {
		t.Fatal("expected non-empty search results")
	}
}

func TestSearchTool_NilStore(t *testing.T) {
	st := NewSearchTool(nil)
	_, err := st.Execute(context.Background(), `{"query": "test"}`)
	if err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestAddTool_Execute(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	at := NewAddTool(store)
	if at.Definition().Name != "knowledge_add" {
		t.Fatalf("expected tool name knowledge_add, got %s", at.Definition().Name)
	}

	result, err := at.Execute(ctx, `{"name":"debug-tips.md","content":"Always check logs first."}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"status":"ok"`) {
		t.Fatalf("expected ok status, got %s", result)
	}

	doc, err := store.GetDocument(ctx, "default", "debug-tips.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Content, "Always check logs first") {
		t.Fatal("document content mismatch")
	}
}

func TestAddTool_CustomDataset(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	at := NewAddTool(store)
	_, err := at.Execute(ctx, `{"dataset_id":"recipes","name":"go-deploy.md","content":"Use multi-stage Docker builds."}`)
	if err != nil {
		t.Fatal(err)
	}

	doc, err := store.GetDocument(ctx, "recipes", "go-deploy.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc == nil {
		t.Fatal("expected document in custom dataset")
	}
}

func TestAddTool_NilStore(t *testing.T) {
	at := NewAddTool(nil)
	_, err := at.Execute(context.Background(), `{"name":"x.md","content":"y"}`)
	if err == nil {
		t.Fatal("expected error for nil store")
	}
}
