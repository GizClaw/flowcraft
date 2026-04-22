package objstore

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// WorkspaceOption configures an object-store backed Workspace.
type WorkspaceOption func(*objectWorkspace)

// WithPrefix sets a key prefix for all operations, providing namespace
// isolation within a single bucket. The prefix should not have a leading
// slash but should end with "/" (it will be normalized).
func WithPrefix(prefix string) WorkspaceOption {
	return func(w *objectWorkspace) {
		p := strings.Trim(prefix, "/")
		if p != "" {
			w.prefix = p + "/"
		}
	}
}

// NewWorkspace creates a workspace.Workspace backed by an ObjectStore.
func NewWorkspace(store ObjectStore, opts ...WorkspaceOption) workspace.Workspace {
	w := &objectWorkspace{store: store}
	for _, o := range opts {
		o(w)
	}
	return w
}

type objectWorkspace struct {
	store  ObjectStore
	prefix string
}

func (w *objectWorkspace) Read(ctx context.Context, path string) ([]byte, error) {
	key, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	data, err := w.store.Get(ctx, key)
	if err != nil {
		return nil, mapErr(path, err)
	}
	return data, nil
}

func (w *objectWorkspace) Write(ctx context.Context, path string, data []byte) error {
	key, err := w.resolve(path)
	if err != nil {
		return err
	}
	return w.store.Put(ctx, key, data)
}

func (w *objectWorkspace) Append(ctx context.Context, path string, data []byte) error {
	key, err := w.resolve(path)
	if err != nil {
		return err
	}
	if a, ok := w.store.(Appender); ok {
		return a.Append(ctx, key, data)
	}
	// Fallback: read-modify-write (not atomic).
	existing, getErr := w.store.Get(ctx, key)
	if getErr != nil {
		existing = nil
	}
	return w.store.Put(ctx, key, append(existing, data...))
}

func (w *objectWorkspace) Rename(ctx context.Context, src, dst string) error {
	srcKey, err := w.resolve(src)
	if err != nil {
		return err
	}
	dstKey, err := w.resolve(dst)
	if err != nil {
		return err
	}
	if srcKey == dstKey {
		return nil
	}
	// Object stores generally lack atomic rename; do copy + delete.
	// Callers that need true atomicity should use a local-fs workspace.
	data, err := w.store.Get(ctx, srcKey)
	if err != nil {
		return mapErr(src, err)
	}
	if err := w.store.Put(ctx, dstKey, data); err != nil {
		return err
	}
	return w.store.Del(ctx, srcKey)
}

func (w *objectWorkspace) Delete(ctx context.Context, path string) error {
	key, err := w.resolve(path)
	if err != nil {
		return err
	}
	return w.store.Del(ctx, key)
}

func (w *objectWorkspace) RemoveAll(ctx context.Context, path string) error {
	if path == "" || path == "." {
		return fmt.Errorf("%w", ErrDeleteRoot)
	}
	key, err := w.resolve(path)
	if err != nil {
		return err
	}
	// Delete the object itself and everything "under" it.
	if delErr := w.store.Del(ctx, key); delErr != nil {
		_ = delErr
	}
	prefix := key
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return w.store.DelPrefix(ctx, prefix)
}

func (w *objectWorkspace) List(ctx context.Context, dir string) ([]fs.DirEntry, error) {
	prefix, err := w.resolveDir(dir)
	if err != nil {
		return nil, err
	}

	result, err := w.store.ListPrefix(ctx, prefix, "/")
	if err != nil {
		return nil, fmt.Errorf("objstore: list %s: %w", dir, err)
	}

	entries := make([]fs.DirEntry, 0, len(result.Objects)+len(result.CommonPrefixes))
	for _, cp := range result.CommonPrefixes {
		name := strings.TrimSuffix(strings.TrimPrefix(cp, prefix), "/")
		if name == "" {
			continue
		}
		entries = append(entries, &objDirEntry{name: name, dir: true})
	}
	for _, obj := range result.Objects {
		name := strings.TrimPrefix(obj.Key, prefix)
		if name == "" {
			continue
		}
		entries = append(entries, &objDirEntry{
			name:    name,
			size:    obj.Size,
			modTime: obj.LastModified,
		})
	}
	return entries, nil
}

func (w *objectWorkspace) Exists(ctx context.Context, path string) (bool, error) {
	key, err := w.resolve(path)
	if err != nil {
		return false, err
	}
	_, headErr := w.store.Head(ctx, key)
	if headErr == nil {
		return true, nil
	}
	// Might be an implicit directory — check if any keys exist with this prefix.
	prefix := key + "/"
	result, listErr := w.store.ListPrefix(ctx, prefix, "/")
	if listErr != nil {
		return false, listErr
	}
	return len(result.Objects) > 0 || len(result.CommonPrefixes) > 0, nil
}

func (w *objectWorkspace) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	key, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	info, headErr := w.store.Head(ctx, key)
	if headErr == nil {
		return &objFileInfo{
			name:    filepath.Base(path),
			size:    info.Size,
			modTime: info.LastModified,
		}, nil
	}
	// Check implicit directory.
	prefix := key + "/"
	result, listErr := w.store.ListPrefix(ctx, prefix, "/")
	if listErr != nil {
		return nil, fmt.Errorf("objstore: stat %s: %w", path, listErr)
	}
	if len(result.Objects) > 0 || len(result.CommonPrefixes) > 0 {
		return &objFileInfo{
			name:    filepath.Base(path),
			dir:     true,
			modTime: time.Now(),
		}, nil
	}
	return nil, fmt.Errorf("%w: %s", workspace.ErrNotFound, path)
}

// resolve converts a workspace-relative path into an object key with prefix.
func (w *objectWorkspace) resolve(path string) (string, error) {
	if path == "" || path == "." {
		return "", fmt.Errorf("%w: empty path not allowed for this operation", ErrInvalidKey)
	}
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, path)
	}
	return w.prefix + filepath.ToSlash(cleaned), nil
}

// resolveDir converts a workspace-relative dir path into a prefix for listing.
func (w *objectWorkspace) resolveDir(dir string) (string, error) {
	if dir == "" || dir == "." {
		return w.prefix, nil
	}
	cleaned := filepath.Clean(dir)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, dir)
	}
	return w.prefix + filepath.ToSlash(cleaned) + "/", nil
}

func mapErr(path string, err error) error {
	if strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("%w: %s", workspace.ErrNotFound, path)
	}
	return err
}

// --- fs.DirEntry / fs.FileInfo implementations ---

type objDirEntry struct {
	name    string
	dir     bool
	size    int64
	modTime time.Time
}

func (e *objDirEntry) Name() string               { return e.name }
func (e *objDirEntry) IsDir() bool                { return e.dir }
func (e *objDirEntry) Type() fs.FileMode          { return e.fileInfo().Mode().Type() }
func (e *objDirEntry) Info() (fs.FileInfo, error) { return e.fileInfo(), nil }
func (e *objDirEntry) fileInfo() *objFileInfo {
	return &objFileInfo{name: e.name, size: e.size, modTime: e.modTime, dir: e.dir}
}

type objFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	dir     bool
}

func (i *objFileInfo) Name() string       { return i.name }
func (i *objFileInfo) Size() int64        { return i.size }
func (i *objFileInfo) ModTime() time.Time { return i.modTime }
func (i *objFileInfo) IsDir() bool        { return i.dir }
func (i *objFileInfo) Sys() any           { return nil }
func (i *objFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
