package fsx

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// WalkFunc is called for each entry beneath a walked directory. path is
// absolute. Returning an error stops the walk.
type WalkFunc func(path string, entry fs.DirEntry) error

// Read returns the whole file. Reads are unrestricted: any absolute path this
// process can open is readable, because no read restriction is projected.
func (t *Tree) Read(ctx context.Context, path string) ([]byte, error) {
	if err := t.checkRead(ctx, path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// ReadStream returns the file as a stream, for payloads too large to hold.
func (t *Tree) ReadStream(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := t.checkRead(ctx, path); err != nil {
		return nil, err
	}
	return os.Open(path)
}

// Stat returns file metadata.
func (t *Tree) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	if err := t.checkRead(ctx, path); err != nil {
		return nil, err
	}
	return os.Stat(path)
}

// List returns the direct children of dir, sorted by name.
func (t *Tree) List(ctx context.Context, dir string) ([]fs.DirEntry, error) {
	if err := t.checkRead(ctx, dir); err != nil {
		return nil, err
	}
	return os.ReadDir(dir)
}

// Walk visits every entry beneath dir in lexicographic order.
func (t *Tree) Walk(ctx context.Context, dir string, fn WalkFunc) error {
	if err := t.checkRead(ctx, dir); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return errdefs.FromContext(err)
		}
		if path == dir {
			return nil
		}
		return fn(path, entry)
	})
}

// Write replaces a file, creating missing parent directories.
//
// It runs in the sandboxed helper, so the kernel decides whether the path is
// writable — this method never decides.
func (t *Tree) Write(ctx context.Context, path string, data []byte) error {
	return t.call(ctx, helperRequest{Op: "write", Path: path, Data: data})
}

// WriteStream replaces a file from a stream and reports the byte count.
//
// The payload is buffered: the helper protocol carries one request, so a
// streaming write would need a second protocol. Reads stream; writes do not.
func (t *Tree) WriteStream(ctx context.Context, path string, src io.Reader) (int64, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return 0, err
	}
	if err := t.Write(ctx, path, data); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// Mkdir creates a directory and its parents. Like every write, it is performed
// inside the sandbox.
func (t *Tree) Mkdir(ctx context.Context, path string) error {
	return t.call(ctx, helperRequest{Op: "mkdir", Path: path})
}

// Remove deletes a file, or a directory tree. It is idempotent: removing a path
// that does not exist returns nil.
func (t *Tree) Remove(ctx context.Context, path string) error {
	return t.call(ctx, helperRequest{Op: "remove", Path: path})
}

// Copy duplicates a file: the source is read here, the destination is written
// through the sandbox, so the boundary applies to the side that persists.
func (t *Tree) Copy(ctx context.Context, src, dst string) error {
	data, err := t.Read(ctx, src)
	if err != nil {
		return err
	}
	return t.Write(ctx, dst, data)
}
