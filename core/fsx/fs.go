package fsx

import (
	"context"
	"io"
	"io/fs"
)

// WalkFunc is called for each entry beneath a walked directory. rel is
// root-relative and slash-separated. Returning an error stops the walk.
type WalkFunc func(rel string, entry fs.DirEntry) error

// FS is the file-I/O contract. Every method takes the per-call Sandbox:
// the boundary travels with the call, so an implementation that runs the
// operation elsewhere can carry it across unchanged.
//
// Implementations must be safe for concurrent use and must treat Sandbox
// as immutable.
type FS interface {
	// Canonicalize resolves path against the sandbox and returns the
	// executor-side absolute form, after any symlink resolution the
	// backend performs.
	Canonicalize(ctx context.Context, path string, sb *Sandbox) (string, error)
	// Read returns the whole file.
	Read(ctx context.Context, path string, sb *Sandbox) ([]byte, error)
	// ReadStream returns the file as a stream, for payloads too large to
	// hold in memory.
	ReadStream(ctx context.Context, path string, sb *Sandbox) (io.ReadCloser, error)
	// Write replaces a file, creating missing parent directories.
	Write(ctx context.Context, path string, data []byte, sb *Sandbox) error
	// WriteStream replaces a file from a stream and reports the byte
	// count written.
	WriteStream(ctx context.Context, path string, src io.Reader, sb *Sandbox) (int64, error)
	// Stat returns file metadata without reading the payload.
	Stat(ctx context.Context, path string, sb *Sandbox) (fs.FileInfo, error)
	// List returns the direct children of dir, sorted by name.
	List(ctx context.Context, dir string, sb *Sandbox) ([]fs.DirEntry, error)
	// Walk visits every entry beneath dir in lexicographic order.
	Walk(ctx context.Context, dir string, sb *Sandbox, fn WalkFunc) error
	// Mkdir creates a directory and its parents.
	Mkdir(ctx context.Context, path string, sb *Sandbox) error
	// Remove deletes a file, or a directory tree.
	Remove(ctx context.Context, path string, sb *Sandbox) error
	// Copy duplicates a file within the tree.
	Copy(ctx context.Context, src, dst string, sb *Sandbox) error
}
