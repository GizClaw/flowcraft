package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestStartWatching_LocalWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := NewFSStore(ws)
	ctx := context.Background()

	w, err := store.StartWatching(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w == nil {
		t.Fatal("expected watcher for local workspace")
	}
	defer w.Stop()

	knowledgeDir := filepath.Join(dir, "knowledge")
	if _, err := os.Stat(knowledgeDir); os.IsNotExist(err) {
		t.Fatal("expected knowledge directory to be created")
	}
}

func TestStartWatching_MemWorkspace(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)

	w, err := store.StartWatching(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w != nil {
		t.Fatal("expected nil watcher for mem workspace")
	}
}

func TestStartWatching_DetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	w, err := store.StartWatching(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	dsDir := filepath.Join(dir, "knowledge", "testds")
	if err := os.MkdirAll(dsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dsDir, "doc.md"), []byte("Go programming language"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	results, err := store.Search(ctx, "testds", "Go programming", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results after file watcher detected new file")
	}
}
