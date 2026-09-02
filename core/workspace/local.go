package workspace

import (
	"context"
	"fmt"
	"io"
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
//
// The top-level workspace pins the resolved root with an *os.Root, so
// every operation resolves paths relative to a directory handle with
// per-component symlink containment (no check-then-use window to race).
// Views created by Sub share that handle and carry a relative prefix;
// only the top-level workspace owns the file descriptor and implements
// Close. Note the os.Root semantics: symlink targets must be relative
// (an absolute symlink is rejected even when it points inside the root),
// and RemoveAll on a final escaping symlink removes the link itself,
// never the target.
type LocalWorkspace struct {
	root   string // display absolute path of this view
	rootFS *os.Root
	prefix string // relative path of this view inside rootFS; "" = top level
}

// NewLocalWorkspace creates a workspace rooted at the given directory.
// The root path is resolved through EvalSymlinks and then pinned with
// os.OpenRoot, so the root itself cannot be swapped for a symlink later.
func NewLocalWorkspace(root string) (*LocalWorkspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: eval symlinks for root: %w", err)
	}
	rootFS, err := os.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("workspace: open root %s: %w", real, err)
	}
	return &LocalWorkspace{root: real, rootFS: rootFS}, nil
}

// Root returns the absolute display path of the workspace view. For a
// Sub view the path is the logical join of the parent root and the
// prefix; it may contain symlink components that the underlying
// os.Root view resolves internally. Path-based consumers should only
// use it for display or one-shot opens while no hostile writer can
// swap components; the Workspace methods themselves stay race-free.
func (w *LocalWorkspace) Root() string { return w.root }

// Close releases the underlying root file descriptor. Views created by
// Sub borrow the top-level handle, so Close is a no-op on them; close
// the top-level workspace only after all views are done.
func (w *LocalWorkspace) Close() error {
	if w == nil || w.rootFS == nil || w.prefix != "" {
		return nil
	}
	return w.rootFS.Close()
}

// Sub returns a local workspace view rooted under prefix.
//
// The view shares the parent's os.Root and prefixes every operation,
// so symlinks inside the prefix may only resolve back inside the root.
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
	rel := filepath.Join(w.prefix, cleaned)
	display := filepath.Join(w.root, cleaned)

	// Symlink-escape classification. This check is advisory only: the
	// security boundary is the os.Root handle used by MkdirAll below.
	if real, rerr := evalExistingPrefix(display); rerr == nil && !containedIn(real, w.rootFS.Name()) {
		return nil, fmt.Errorf("%w: %s (symlink escape)", ErrPathTraversal, cleaned)
	}
	if err := w.rootFS.MkdirAll(rel, 0o700); err != nil {
		return nil, fmt.Errorf("workspace local sub: create %q: %w", cleaned, err)
	}
	return &LocalWorkspace{root: display, rootFS: w.rootFS, prefix: rel}, nil
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

// relPath validates path and maps it to a path relative to the pinned
// os.Root. "" and "." both mean the root of this view.
func (w *LocalWorkspace) relPath(path string) (string, error) {
	cleaned, err := cleanPath(path)
	if err != nil {
		return "", err
	}
	if cleaned == "" {
		cleaned = "."
	}
	return filepath.Join(w.prefix, cleaned), nil
}

func (w *LocalWorkspace) Read(_ context.Context, path string) ([]byte, error) {
	rel, err := w.relPath(path)
	if err != nil {
		return nil, err
	}
	data, err := w.rootFS.ReadFile(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("workspace: read %s: %w", path, err)
	}
	return data, nil
}

// ReadLimited implements [LimitedReader]. It opens the file through the
// pinned os.Root and drains at most maxBytes+1 bytes, so a file that
// grows concurrently cannot force an oversized allocation.
func (w *LocalWorkspace) ReadLimited(_ context.Context, path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errdefs.Validationf("workspace: ReadLimited maxBytes must be positive")
	}
	rel, err := w.relPath(path)
	if err != nil {
		return nil, err
	}
	f, err := w.rootFS.Open(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("workspace: read %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			telemetry.WarnErr(context.Background(), "workspace: close after limited read", cerr,
				otellog.String("op", "read_limited"),
				otellog.String("path", path))
		}
	}()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("workspace: read %s: %w", path, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errdefs.Validationf(
			"workspace: %s exceeds %d bytes", path, maxBytes)
	}
	return data, nil
}

func (w *LocalWorkspace) Write(_ context.Context, path string, data []byte) error {
	rel, err := w.relPath(path)
	if err != nil {
		return err
	}
	if err := w.rootFS.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", path, err)
	}
	if err := w.rootFS.WriteFile(rel, data, 0o600); err != nil {
		return fmt.Errorf("workspace: write %s: %w", path, err)
	}
	return nil
}

func (w *LocalWorkspace) Append(ctx context.Context, path string, data []byte) error {
	rel, err := w.relPath(path)
	if err != nil {
		return err
	}
	if err := w.rootFS.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", path, err)
	}
	f, err := w.rootFS.OpenFile(rel, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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
	srcRel, err := w.relPath(src)
	if err != nil {
		return err
	}
	dstRel, err := w.relPath(dst)
	if err != nil {
		return err
	}
	if err := w.rootFS.MkdirAll(filepath.Dir(dstRel), 0o700); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", dst, err)
	}
	if runtime.GOOS == "windows" {
		// MoveFileEx's REPLACE_EXISTING cannot replace a directory, so
		// renaming over an existing destination directory fails with a
		// platform-specific error where POSIX rename(2) would replace an
		// empty directory. Directory rename is outside the Workspace
		// contract; give a clear error for the divergent case instead of
		// leaking Windows error codes. Moving a directory to a new name
		// still works and is left alone.
		if srcInfo, statErr := w.rootFS.Stat(srcRel); statErr == nil {
			if dstInfo, dstErr := w.rootFS.Stat(dstRel); dstErr == nil {
				if srcInfo.IsDir() || dstInfo.IsDir() {
					return errdefs.Validationf(
						"workspace: rename %s -> %s: replacing a directory is not supported",
						src, dst)
				}
			} else if !os.IsNotExist(dstErr) {
				return fmt.Errorf("workspace: stat rename destination %s: %w", dst, dstErr)
			}
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("workspace: stat rename source %s: %w", src, statErr)
		}
	}
	if err := w.rootFS.Rename(srcRel, dstRel); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, src)
		}
		return fmt.Errorf("workspace: rename %s -> %s: %w", src, dst, err)
	}
	return nil
}

func (w *LocalWorkspace) Delete(_ context.Context, path string) error {
	rel, err := w.relPath(path)
	if err != nil {
		return err
	}
	if info, statErr := w.rootFS.Stat(rel); statErr == nil {
		if info.IsDir() {
			return errdefs.Validationf("workspace: %s is a directory (use RemoveAll)", path)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("workspace: delete %s: %w", path, statErr)
	}
	if err := removeWithRetry(func() error { return w.rootFS.Remove(rel) }); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("workspace: delete %s: %w", path, err)
	}
	return nil
}

func (w *LocalWorkspace) RemoveAll(_ context.Context, path string) error {
	cleaned, err := cleanPath(path)
	if err != nil {
		return err
	}
	if cleaned == "" {
		return errdefs.Forbiddenf("workspace: refusing to remove root")
	}
	rel := filepath.Join(w.prefix, cleaned)
	if err := removeWithRetry(func() error { return w.rootFS.RemoveAll(rel) }); err != nil {
		return fmt.Errorf("workspace: remove all %s: %w", path, err)
	}
	return nil
}

func (w *LocalWorkspace) List(_ context.Context, dir string) ([]fs.DirEntry, error) {
	rel, err := w.relPath(dir)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(w.rootFS.FS(), filepath.ToSlash(rel))
	if err != nil {
		if os.IsNotExist(err) {
			return []fs.DirEntry{}, nil
		}
		return nil, fmt.Errorf("workspace: list %s: %w", dir, err)
	}
	return entries, nil
}

func (w *LocalWorkspace) Exists(_ context.Context, path string) (bool, error) {
	rel, err := w.relPath(path)
	if err != nil {
		return false, err
	}
	_, err = w.rootFS.Stat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("workspace: exists %s: %w", path, err)
	}
	return true, nil
}

func (w *LocalWorkspace) Stat(_ context.Context, path string) (fs.FileInfo, error) {
	rel, err := w.relPath(path)
	if err != nil {
		return nil, err
	}
	info, err := w.rootFS.Stat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("workspace: stat %s: %w", path, err)
	}
	return info, nil
}

// containedIn reports whether path is root itself or directly under
// it. Paths are case-insensitive on Windows, so the prefix check must
// fold case there; on case-sensitive filesystems it is byte-exact.
// Used only to classify errors; the os.Root handle is the boundary.
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

// evalExistingPrefix resolves symlinks for the longest existing ancestor
// of path, then appends the remaining non-existent tail. Used only for
// error classification of Sub; enforcement stays in os.Root.
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
