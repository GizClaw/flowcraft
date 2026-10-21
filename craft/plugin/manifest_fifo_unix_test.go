//go:build !windows

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestManifestRejectsNonRegularCommand pins the "regular file" half of
// the exec check with a file type exec cannot run at all. A file that
// exists is not enough: exec wants something it can map and execute.
func TestManifestRejectsNonRegularCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "bin", "server")
	if err := os.MkdirAll(filepath.Dir(fifo), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	manifest, err := ParseManifest(context.Background(), []byte(`{
		"id": "hello", "version": "0.1.0",
		"permissions": ["tools:provide"],
		"run": {"command": "bin/server"}
	}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	err = manifest.Validate(dir)
	if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("Validate = %v, want a regular-file error", err)
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("Validate = %v, want a Validation error", err)
	}
}
