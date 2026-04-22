// Package workspace provides a file-system sandbox abstraction.
// Knowledge, Skills, and Memory subsystems share a single Workspace
// to manage persistent state as a unified file tree.
package workspace

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Workspace abstracts file operations over a sandboxed directory tree.
// All paths are relative to the workspace root; absolute paths and path
// traversals ("..") are rejected.
type Workspace interface {
	Read(ctx context.Context, path string) ([]byte, error)
	Write(ctx context.Context, path string, data []byte) error
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
	Rename(ctx context.Context, src, dst string) error
	Delete(ctx context.Context, path string) error
	RemoveAll(ctx context.Context, path string) error
	List(ctx context.Context, dir string) ([]fs.DirEntry, error)
	Exists(ctx context.Context, path string) (bool, error)
	Stat(ctx context.Context, path string) (fs.FileInfo, error)
}

// GitWorkspace extends Workspace with Git operations.
type GitWorkspace interface {
	Workspace
	GitClone(ctx context.Context, url, dest string) error
	GitPull(ctx context.Context, dir string) error
	GitHead(ctx context.Context, dir string) (string, error)
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
