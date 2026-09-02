// Package workspace provides a persistent, filesystem-shaped storage
// abstraction for agent state.
//
// Workspace is the pluggable backend for agent file operations (sandboxed
// scripts, bridge files) and for subsystems such as memory, including its
// knowledge line. Higher layers depend on the Workspace interface and never
// on a concrete implementation. Implementations include an in-memory map, a
// local directory tree, prefixed and scoped views, and object stores.
// Execution-boundary concerns (command runners, network policy, resource
// limits) live in sibling package core/sandbox.
package workspace

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Workspace abstracts file operations over a sandboxed directory tree.
//
// All implementations agree on the following contract:
//   - Paths are relative to the workspace root; absolute paths and path
//     traversals ("..") are rejected with ErrPathTraversal.
//   - Write and Append create missing parent directories and fail when path
//     is an existing directory. Neither is atomic: a concurrent reader may
//     observe a partial payload. Callers publish finalized data with
//     AtomicWrite or Rename.
//   - Rename is the canonical "publish a finalized payload" operation.
//   - Delete and RemoveAll are idempotent: removing a missing path returns
//     nil. Delete operates on files only; directories require RemoveAll.
//   - List returns direct children in lexicographic order by Name. A missing
//     or empty directory returns an empty slice, not an error.
//   - Read, Stat, and the source of Rename return ErrNotFound for missing
//     paths.
type Workspace interface {
	// Read returns the full contents of path, or ErrNotFound when path
	// does not exist.
	Read(ctx context.Context, path string) ([]byte, error)
	// Write stores data at path, creating missing parent directories. It
	// fails when path is an existing directory. Write is not atomic; use
	// AtomicWrite or Rename to publish a payload that readers can never
	// observe half-written.
	Write(ctx context.Context, path string, data []byte) error
	// Append appends data to path, creating the file and missing parent
	// directories when needed. It fails when path is an existing directory.
	// Append is not atomic and must not be used to publish a finalized
	// payload.
	Append(ctx context.Context, path string, data []byte) error
	// Rename moves src to dst within the same workspace. Implementations
	// MUST be atomic when the underlying medium supports it (e.g. POSIX
	// rename(2) on a local filesystem). When the medium cannot rename
	// atomically (e.g. object stores) the implementation MAY fall back
	// to copy + delete, but callers should treat Rename as the canonical
	// "publish a finalized payload" operation: write to a tmp path then
	// Rename to the live path so readers never observe a half-written file.
	//
	// Returns ErrNotFound if src does not exist. Overwriting an existing
	// dst is allowed; on local filesystems this is atomic.
	// Rename moves files; directory rename is not part of the contract and
	// implementations may reject it.
	Rename(ctx context.Context, src, dst string) error
	// Delete removes the file at path and never removes directory contents.
	// It is idempotent: deleting a path that does not exist returns nil.
	// Backends with a native directory concept reject deleting a directory;
	// use RemoveAll for trees.
	Delete(ctx context.Context, path string) error
	// RemoveAll recursively removes the directory tree at path and is
	// idempotent for missing paths. Removing the workspace root is
	// rejected.
	RemoveAll(ctx context.Context, path string) error
	// List returns the direct children of dir in lexicographic order by
	// Name. A missing or empty directory returns an empty slice with nil
	// error. Directory entries (including implicit directories synthesized
	// from nested paths) report IsDir.
	List(ctx context.Context, dir string) ([]fs.DirEntry, error)
	// Exists reports whether path exists, without returning an error for
	// missing paths.
	Exists(ctx context.Context, path string) (bool, error)
	// Stat returns metadata for path, or ErrNotFound when path does not
	// exist.
	Stat(ctx context.Context, path string) (fs.FileInfo, error)
}

// LimitedReader is implemented by Workspace backends that can bound a
// single read. ReadLimited MUST NOT materialize more than maxBytes
// bytes in memory; an oversized file fails with a bounded read error
// instead of returning the full payload.
type LimitedReader interface {
	ReadLimited(ctx context.Context, path string, maxBytes int64) ([]byte, error)
}

// ViolationRecord captures a rejected operation for audit logging.
type ViolationRecord struct {
	Time      time.Time `json:"time"`
	Operation string    `json:"operation"`
	Path      string    `json:"path"`
	Reason    string    `json:"reason"`
}

// ViolationLogger receives violation records from ScopedWorkspace.
type ViolationLogger interface {
	LogViolation(ctx context.Context, record ViolationRecord)
}

// Common errors.
var (
	ErrPathTraversal = errdefs.Forbidden(errors.New("workspace: path traversal denied"))
	ErrAccessDenied  = errdefs.Forbidden(errors.New("workspace: access denied"))
	ErrNotFound      = errdefs.NotFound(errors.New("workspace: not found"))
)
