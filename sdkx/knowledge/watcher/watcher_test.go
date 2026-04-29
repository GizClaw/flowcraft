//lint:file-ignore SA1019 watcher itself is Deprecated; the tests
// intentionally exercise the deprecated New / Notifier / NewReloader /
// FSStore surface to keep the v0.2.x compatibility window honest. A
// fresh test suite ships alongside the v0.3.0 EventNotifier rewrite.

package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	watcherpkg "github.com/GizClaw/flowcraft/sdkx/knowledge/watcher"
)

func TestNew_LocalWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := knowledge.NewFSStore(ws)
	ctx := context.Background()

	n, err := watcherpkg.New(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if n == nil {
		t.Fatal("expected notifier for local workspace")
	}
	defer n.Close()

	knowledgeDir := filepath.Join(dir, "knowledge")
	if _, err := os.Stat(knowledgeDir); os.IsNotExist(err) {
		t.Fatal("expected knowledge directory to be created")
	}
}

func TestNew_MemWorkspace(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := knowledge.NewFSStore(ws)

	n, err := watcherpkg.New(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Fatal("expected nil notifier for mem workspace")
	}
}

func TestReloader_DetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := knowledge.NewFSStore(ws, knowledge.WithChunkConfig(knowledge.ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n, err := watcherpkg.New(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	r := knowledge.NewReloader(store, n, knowledge.ReloaderOptions{Debounce: 200 * time.Millisecond})
	go r.Run(ctx)
	defer r.Close()

	dsDir := filepath.Join(dir, "knowledge", "testds")
	if err := os.MkdirAll(dsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dsDir, "doc.md"), []byte("Go programming language"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	results, err := store.Search(ctx, "testds", "Go programming", knowledge.SearchOptions{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results after notifier triggered rebuild")
	}
}
