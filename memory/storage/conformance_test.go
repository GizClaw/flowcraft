package storage_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/workspace"
	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/memory/storage/internal/storagetest"
)

// TestWorkspaceDriverConformance runs the shared driver suite against the
// workspace-backed Log and KV.
func TestWorkspaceDriverConformance(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, storagetest.Backend{Log: logStore, KV: kvStore})
}
