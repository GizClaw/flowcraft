package fsx

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
)

// Local is the filesystem implementation backed by one directory tree.
//
// Every path a caller supplies is resolved against the sandbox root, and the
// resolved absolute path is checked twice: once lexically (permission.Within,
// which refuses ".." escapes and absolute paths outside the tree) and once
// with symlinks resolved (permission.RealWithin, which refuses a link inside
// the tree that points outside it). Writes additionally require the boundary
// to allow them; reads are unrestricted within the tree, because no sandbox
// backend can enforce a read restriction and a permission the kernel cannot
// enforce is not modelled.
type Local struct {
	root     string
	writable []string
}

var _ FS = (*Local)(nil)

// NewLocal returns a filesystem rooted at dir, which must be absolute. The
// directory is created when missing: a host application that names a workspace
// directory should not have to create it before starting.
func NewLocal(dir string, opts ...Option) (*Local, error) {
	if dir == "" {
		return nil, errdefs.Validationf("fsx: local root is empty")
	}
	if !filepath.IsAbs(dir) {
		return nil, errdefs.Validationf("fsx: local root %q must be absolute", dir)
	}
	o, err := buildOptions(opts)
	if err != nil {
		return nil, err
	}
	root := filepath.Clean(dir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("fsx: create root: %w", err)
	}
	writable := o.writable
	if len(writable) == 0 {
		writable = []string{root}
	}
	return &Local{root: root, writable: writable}, nil
}

// Root returns the absolute directory this filesystem is rooted at.
func (l *Local) Root() string { return l.root }

// resolve turns a caller path into an absolute path inside the tree.
func (l *Local) resolve(path string, sb *Sandbox) (string, error) {
	if path == "" {
		return "", errdefs.Validationf("fsx: empty path")
	}
	root := l.root
	cwd := root
	if sb != nil {
		if r := sb.Root(); r != "" {
			root = filepath.Clean(r)
		}
		if sb.Cwd != "" {
			cwd = filepath.Clean(sb.Cwd)
		}
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, path)
	}
	abs = filepath.Clean(abs)
	if !permission.Within(root, abs) {
		return "", denied(abs, "outside "+root)
	}
	// The lexical check accepts a path that does not exist yet; the symlink
	// check resolves the longest existing ancestor, so a link planted inside
	// the tree cannot be used to escape it.
	if !permission.RealWithin(root, abs) {
		return "", denied(abs, "resolves outside "+root)
	}
	return abs, nil
}

// readablePath resolves path and additionally requires read permission.
func (l *Local) readablePath(path string, sb *Sandbox) (string, error) {
	abs, err := l.resolve(path, sb)
	if err != nil {
		return "", err
	}
	if !readable(abs, sb) {
		return "", denied(abs, "not readable under the policy")
	}
	return abs, nil
}

// writablePath resolves path and additionally requires write permission.
func (l *Local) writablePath(path string, sb *Sandbox) (string, error) {
	abs, err := l.resolve(path, sb)
	if err != nil {
		return "", err
	}
	if !writable(abs, sb, l.writable) {
		return "", denied(abs, "not writable under the policy")
	}
	return abs, nil
}

func denied(path, reason string) error {
	return fmt.Errorf("%w: %s: %s", ErrDenied, path, reason)
}

// Canonicalize resolves path and returns its absolute form, following
// symlinks when the path exists.
func (l *Local) Canonicalize(_ context.Context, path string, sb *Sandbox) (string, error) {
	abs, err := l.resolve(path, sb)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// Read returns the whole file.
func (l *Local) Read(_ context.Context, path string, sb *Sandbox) ([]byte, error) {
	abs, err := l.readablePath(path, sb)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// ReadStream returns the file as a stream.
func (l *Local) ReadStream(_ context.Context, path string, sb *Sandbox) (io.ReadCloser, error) {
	abs, err := l.readablePath(path, sb)
	if err != nil {
		return nil, err
	}
	return os.Open(abs)
}

// Write replaces a file, creating missing parent directories, and publishes it
// atomically: a concurrent reader sees the old content or the new content, not
// a half-written file.
func (l *Local) Write(_ context.Context, path string, data []byte, sb *Sandbox) error {
	abs, err := l.writablePath(path, sb)
	if err != nil {
		return err
	}
	return writeAtomic(abs, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteStream replaces a file from a stream, atomically, and reports the byte
// count written.
func (l *Local) WriteStream(_ context.Context, path string, src io.Reader, sb *Sandbox) (int64, error) {
	abs, err := l.writablePath(path, sb)
	if err != nil {
		return 0, err
	}
	var written int64
	if err := writeAtomic(abs, func(w io.Writer) error {
		n, err := io.Copy(w, src)
		written = n
		return err
	}); err != nil {
		return 0, err
	}
	return written, nil
}

// writeAtomic writes through a temporary file in the destination directory and
// renames it over the target.
func writeAtomic(abs string, fill func(io.Writer) error) error {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".fsx-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeded

	if err := fill(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, abs)
}

// Stat returns file metadata.
func (l *Local) Stat(_ context.Context, path string, sb *Sandbox) (fs.FileInfo, error) {
	abs, err := l.readablePath(path, sb)
	if err != nil {
		return nil, err
	}
	return os.Stat(abs)
}

// List returns the direct children of dir, sorted by name.
func (l *Local) List(_ context.Context, dir string, sb *Sandbox) ([]fs.DirEntry, error) {
	abs, err := l.readablePath(dir, sb)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Walk visits every entry beneath dir in lexicographic order.
func (l *Local) Walk(ctx context.Context, dir string, sb *Sandbox, fn WalkFunc) error {
	abs, err := l.readablePath(dir, sb)
	if err != nil {
		return err
	}
	return filepath.WalkDir(abs, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		return fn(filepath.ToSlash(rel), entry)
	})
}

// Mkdir creates a directory and its parents.
func (l *Local) Mkdir(_ context.Context, path string, sb *Sandbox) error {
	abs, err := l.writablePath(path, sb)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755)
}

// Remove deletes a file, or a directory tree. It is idempotent: removing a
// path that does not exist returns nil, so a caller cleaning up does not have
// to distinguish "already gone" from "never existed".
func (l *Local) Remove(_ context.Context, path string, sb *Sandbox) error {
	abs, err := l.writablePath(path, sb)
	if err != nil {
		return err
	}
	if abs == l.root {
		return errdefs.Validationf("fsx: refusing to remove the root")
	}
	err = os.RemoveAll(abs)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Copy duplicates a file within the tree.
func (l *Local) Copy(_ context.Context, src, dst string, sb *Sandbox) error {
	srcAbs, err := l.readablePath(src, sb)
	if err != nil {
		return err
	}
	dstAbs, err := l.writablePath(dst, sb)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(srcAbs)
	if err != nil {
		return err
	}
	return writeAtomic(dstAbs, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}
