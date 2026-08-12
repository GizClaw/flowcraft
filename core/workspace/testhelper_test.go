package workspace

import (
	"context"
	"testing"
)

// newTestWorkspace returns a fresh local workspace on a temporary
// directory. It is the shared test base after the in-memory workspace
// implementation was dropped.
func newTestWorkspace(t *testing.T) Workspace {
	t.Helper()
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	return ws
}

// mustWrite writes data into ws, failing the test on error.
func mustWrite(t *testing.T, ws Workspace, path string, data []byte) {
	t.Helper()
	if err := ws.Write(context.Background(), path, data); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
