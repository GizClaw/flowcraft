package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// LocalWorkspace implements Workspace backed by a local directory.
type LocalWorkspace struct {
	root string
}

// NewLocalWorkspace creates a workspace rooted at the given directory.
// The root path is resolved through EvalSymlinks to prevent the root
// itself from being a symlink that could be swapped later.
func NewLocalWorkspace(root string) (*LocalWorkspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: eval symlinks for root: %w", err)
	}
	return &LocalWorkspace{root: real}, nil
}

// Root returns the absolute path of the workspace root.
func (w *LocalWorkspace) Root() string { return w.root }

// Sub returns a local workspace rooted under prefix.
//
// The resolved child root must remain inside the current workspace, including
// after symlink resolution performed by NewLocalWorkspace.
func (w *LocalWorkspace) Sub(prefix string) (*LocalWorkspace, error) {
	if w == nil {
		return nil, errdefs.Validationf("workspace: local workspace is nil")
	}
	cleaned, err := cleanPath(prefix)
	if err != nil {
		return nil, fmt.Errorf("workspace local sub: invalid prefix %q: %w", prefix, err)
	}
	if cleaned == "" {
		return w, nil
	}
	full, err := w.resolve(cleaned)
	if err != nil {
		return nil, fmt.Errorf("workspace local sub: resolve root %q: %w", cleaned, err)
	}
	local, err := NewLocalWorkspace(full)
	if err != nil {
		return nil, fmt.Errorf("workspace local sub: open root %q: %w", cleaned, err)
	}
	root := local.Root()
	if !containedIn(root, w.Root()) {
		return nil, fmt.Errorf("%w: %s (symlink escape)", ErrPathTraversal, cleaned)
	}
	return local, nil
}

// Capabilities reports LocalWorkspace's storage characteristics:
// backed by the host filesystem, so Rename is atomic on the same
// device and writes are read-after-write consistent. DurableOnWrite
// is false because writes are not fsync'd before returning — a
// successful Write only reaches the OS page cache. Distributed is
// false because LocalWorkspace assumes exclusive access to its
// directory tree.
func (w *LocalWorkspace) Capabilities() Capabilities {
	return Capabilities{
		AtomicRename:   true,
		ReadAfterWrite: true,
		DurableOnWrite: false,
		Distributed:    false,
	}
}

func (w *LocalWorkspace) Read(_ context.Context, path string) ([]byte, error) {
	full, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("workspace: read %s: %w", path, err)
	}
	return data, nil
}

func (w *LocalWorkspace) Write(_ context.Context, path string, data []byte) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", path, err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return fmt.Errorf("workspace: write %s: %w", path, err)
	}
	return nil
}

func (w *LocalWorkspace) Append(ctx context.Context, path string, data []byte) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", path, err)
	}
	f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("workspace: append %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			telemetry.WarnErr(ctx, "workspace: close after append", cerr,
				otellog.String("op", "append"),
				otellog.String("path", path))
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("workspace: append %s: %w", path, err)
	}
	return nil
}

func (w *LocalWorkspace) Rename(_ context.Context, src, dst string) error {
	srcFull, err := w.resolve(src)
	if err != nil {
		return err
	}
	dstFull, err := w.resolve(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstFull), 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", dst, err)
	}
	if err := os.Rename(srcFull, dstFull); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, src)
		}
		return fmt.Errorf("workspace: rename %s -> %s: %w", src, dst, err)
	}
	return nil
}

func (w *LocalWorkspace) Delete(_ context.Context, path string) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(full); statErr == nil {
		if info.IsDir() {
			return errdefs.Validationf("workspace: %s is a directory (use RemoveAll)", path)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("workspace: delete %s: %w", path, statErr)
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("workspace: delete %s: %w", path, err)
	}
	return nil
}

func (w *LocalWorkspace) RemoveAll(_ context.Context, path string) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if full == w.root {
		return errdefs.Forbiddenf("workspace: refusing to remove root")
	}
	return os.RemoveAll(full)
}

func (w *LocalWorkspace) List(_ context.Context, dir string) ([]fs.DirEntry, error) {
	full, err := w.resolve(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return []fs.DirEntry{}, nil
		}
		return nil, fmt.Errorf("workspace: list %s: %w", dir, err)
	}
	return entries, nil
}

func (w *LocalWorkspace) Exists(_ context.Context, path string) (bool, error) {
	full, err := w.resolve(path)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("workspace: exists %s: %w", path, err)
	}
	return true, nil
}

func (w *LocalWorkspace) Stat(_ context.Context, path string) (fs.FileInfo, error) {
	full, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("workspace: stat %s: %w", path, err)
	}
	return info, nil
}

func (w *LocalWorkspace) resolve(path string) (string, error) {
	if path == "" || path == "." {
		return w.root, nil
	}
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, path)
	}
	full := filepath.Join(w.root, cleaned)
	if !containedIn(full, w.root) {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, path)
	}

	// Resolve symlinks for the longest existing prefix to detect escapes
	// through symlinked intermediate directories or files.
	real, err := evalExistingPrefix(full)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve %s: %w", path, err)
	}
	if !containedIn(real, w.root) {
		return "", fmt.Errorf("%w: %s (symlink escape)", ErrPathTraversal, path)
	}
	return full, nil
}

// containedIn reports whether path is root itself or directly under
// it. Paths are case-insensitive on Windows, so the prefix check must
// fold case there; on case-sensitive filesystems it is byte-exact.
func containedIn(path, root string) bool {
	if path == root {
		return true
	}
	if runtime.GOOS == "windows" {
		return strings.HasPrefix(strings.ToLower(path),
			strings.ToLower(root)+string(filepath.Separator))
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// evalExistingPrefix resolves symlinks for the longest existing ancestor of
// path, then appends the remaining non-existent tail. This correctly catches
// symlink escapes even when the final target doesn't exist yet (e.g. Write
// to a new file under a symlinked directory).
func evalExistingPrefix(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	realParent, err := evalExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}
