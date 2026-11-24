package fsx

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// WalkFunc is called for each entry beneath a walked directory. path is
// absolute. Returning an error stops the walk.
type WalkFunc func(path string, entry fs.DirEntry) error

// Read returns a whole file.
//
// The read side decides where the bytes come from: through the handle of the
// root that contains the path, or — when the tree does not restrict reads —
// straight from the os package.
func (t *Tree) Read(ctx context.Context, path string) ([]byte, error) {
	target, err := t.readable(ctx, "read", path)
	if err != nil {
		return nil, err
	}
	data, err := target.readFile()
	return data, denied(err)
}

// ReadStream returns a file as a stream, for payloads too large to hold.
func (t *Tree) ReadStream(ctx context.Context, path string) (io.ReadCloser, error) {
	target, err := t.readable(ctx, "read", path)
	if err != nil {
		return nil, err
	}
	file, err := target.open()
	return file, denied(err)
}

// Stat returns file metadata.
func (t *Tree) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	target, err := t.readable(ctx, "stat", path)
	if err != nil {
		return nil, err
	}
	info, err := target.stat()
	return info, denied(err)
}

// List returns the direct children of dir, sorted by name.
func (t *Tree) List(ctx context.Context, dir string) ([]fs.DirEntry, error) {
	target, err := t.readable(ctx, "list", dir)
	if err != nil {
		return nil, err
	}
	entries, err := target.readDir()
	return entries, denied(err)
}

// Walk visits every entry beneath dir in lexicographic order, skipping dir
// itself. A context cancellation stops the walk with the context's error.
func (t *Tree) Walk(ctx context.Context, dir string, fn WalkFunc) error {
	target, err := t.readable(ctx, "walk", dir)
	if err != nil {
		return err
	}
	if target.root == nil {
		return filepath.WalkDir(target.name, func(path string, entry fs.DirEntry, err error) error {
			return walkStep(ctx, path, target.name, entry, err, fn)
		})
	}
	return fs.WalkDir(target.root.FS(), target.fsName(), func(path string, entry fs.DirEntry, err error) error {
		absolute := filepath.Join(target.base, filepath.FromSlash(path))
		return walkStep(ctx, absolute, target.abs(), entry, err, fn)
	})
}

func walkStep(ctx context.Context, path, root string, entry fs.DirEntry, err error, fn WalkFunc) error {
	if err != nil {
		return denied(err)
	}
	if err := ctx.Err(); err != nil {
		return errdefs.FromContext(err)
	}
	if path == root {
		return nil
	}
	return fn(path, entry)
}

// Write replaces a file, creating missing parent directories.
//
// The write lands on the handle of the writable root that contains path, so the
// kernel — not this method — decides whether it stays inside. A path that
// matches no root is refused before anything is opened.
func (t *Tree) Write(ctx context.Context, path string, data []byte) error {
	_, err := t.WriteStream(ctx, path, bytes.NewReader(data))
	return err
}

// WriteStream replaces a file from a stream and reports the byte count.
//
// It copies straight into the file: memory stays at the copy buffer regardless
// of payload size.
func (t *Tree) WriteStream(ctx context.Context, path string, src io.Reader) (int64, error) {
	target, err := t.writable(ctx, "write", path)
	if err != nil {
		return 0, err
	}
	if parent, needed := target.parent(); needed {
		if err := parent.mkdirAll(); err != nil {
			return 0, denied(err)
		}
	}
	file, err := target.create()
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
	target, err := t.writable(ctx, "mkdir", path)
	if err != nil {
		return err
	}
	return denied(target.mkdirAll())
}

// Remove deletes a file, or a directory tree. It is idempotent: removing a path
// that does not exist returns nil. Removing a writable root itself is refused —
// the root is the boundary, not content inside it.
func (t *Tree) Remove(ctx context.Context, path string) error {
	target, err := t.writable(ctx, "remove", path)
	if err != nil {
		return err
	}
	if target.root != nil && target.name == "." {
		return errdefs.Validationf("fsx: refusing to remove the writable root %q", target.base)
	}
	return denied(target.removeAll())
}

// Copy duplicates a file: the source is read through the read side, the
// destination is written through the write side, so each boundary applies to the
// half that touches it. It streams, so a large file never becomes one buffer.
func (t *Tree) Copy(ctx context.Context, src, dst string) error {
	if src == dst {
		return errdefs.Validationf("fsx: copy source and destination are the same")
	}
	in, err := t.ReadStream(ctx, src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	_, err = t.WriteStream(ctx, dst, in)
	return err
}
