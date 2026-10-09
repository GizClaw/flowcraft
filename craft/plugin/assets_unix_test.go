//go:build !windows

package plugin

import (
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
	"testing"
)

// TestAssetFSRefusesNonRegularFiles covers the file that would hang the
// host rather than fail it: opening a fifo blocks until a writer
// appears, so the mode is checked before the open.
func TestAssetFSRefusesNonRegularFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pluginDir := writeUIPlugin(t, root, "hello")
	if err := syscall.Mkfifo(filepath.Join(pluginDir, "dist", "pipe.js"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	host := newAssetHost(t, root)
	assets, ok := host.AssetFS("hello")
	if !ok {
		t.Fatal("AssetFS(hello) = false, want a bundle view")
	}
	if _, err := assets.Open("pipe.js"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("open pipe.js = %v, want fs.ErrInvalid", err)
	}
	if _, err := fs.ReadFile(assets, "pipe.js"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("read pipe.js = %v, want fs.ErrInvalid", err)
	}
}
