package fsx

import (
	"bytes"
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

// Read returns the whole file. Reads are unrestricted by decision: any absolute
// path this process can open is readable, and the boundary that matters for
// writes is not a boundary for reads.
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
// The write happens on the handle of the writable root that contains path, so
// the kernel — not this method — decides whether it stays inside. A path that
// matches no root is refused before anything is opened.
func (t *Tree) Write(ctx context.Context, path string, data []byte) error {
	_, err := t.WriteStream(ctx, path, bytes.NewReader(data))
	return err
}

// WriteStream replaces a file from a stream and reports the byte count.
//
// It copies straight into the file handle: memory stays at the copy buffer
// regardless of payload size. (An earlier design buffered the whole payload
// because a helper protocol carried one request; with a handle there is no such
// constraint, and buffering would cap the reachable size at the process limit.)
func (t *Tree) WriteStream(ctx context.Context, path string, src io.Reader) (int64, error) {
	handle, rel, err := t.writable(ctx, "write", path)
	if err != nil {
		return 0, err
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := handle.MkdirAll(dir, 0o755); err != nil {
			return 0, denied(err)
		}
	}
	file, err := handle.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, denied(err)
	}
	written, copyErr := io.Copy(file, src)
	closeErr := file.Close()
	if copyErr != nil {
		return written, denied(copyErr)
	}
	return written, denied(closeErr)
}

// Mkdir creates a directory and its parents inside a writable root.
func (t *Tree) Mkdir(ctx context.Context, path string) error {
	handle, rel, err := t.writable(ctx, "mkdir", path)
	if err != nil {
		return err
	}
	return denied(handle.MkdirAll(rel, 0o755))
}

// Remove deletes a file, or a directory tree. It is idempotent: removing a path
// that does not exist returns nil.
func (t *Tree) Remove(ctx context.Context, path string) error {
	handle, rel, err := t.writable(ctx, "remove", path)
	if err != nil {
		return err
	}
	if rel == "." {
		return errdefs.Validationf("fsx: refusing to remove the writable root %q", path)
	}
	return denied(handle.RemoveAll(rel))
}

// Copy duplicates a file: the source is read here, the destination is written
// through a writable root, so the boundary applies to the side that persists.
func (t *Tree) Copy(ctx context.Context, src, dst string) error {
	data, err := t.Read(ctx, src)
	if err != nil {
		return err
	}
	return t.Write(ctx, dst, data)
}

// writable resolves an absolute target to the handle and relative path that
// carry out the operation.
func (t *Tree) writable(ctx context.Context, op, path string) (*os.Root, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", errdefs.FromContext(err)
	}
	if err := absolute(op, path); err != nil {
		return nil, "", err
	}
	handle, rel, err := t.select_(path)
	if err != nil {
		return nil, "", err
	}
	return handle.root, rel, nil
}
