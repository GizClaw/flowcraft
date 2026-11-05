package fsx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/permission"
)

// Mem is the in-memory filesystem implementation. It exists so callers can be
// tested without a disk: the FS contract is the same, the policy checks are the
// same, and nothing about a test's behaviour depends on the host.
//
// Paths are logical: they are rooted at a virtual directory (default /mem) and
// never touch the real filesystem, so an absolute path here is not a host path.
type Mem struct {
	mu       sync.RWMutex
	root     string
	files    map[string][]byte
	dirs     map[string]bool
	writable []string
}

var _ FS = (*Mem)(nil)

// NewMem returns an empty in-memory filesystem rooted at /mem.
func NewMem(opts ...Option) (*Mem, error) {
	o, err := buildOptions(opts)
	if err != nil {
		return nil, err
	}
	const root = "/mem"
	writable := o.writable
	if len(writable) == 0 {
		writable = []string{root}
	}
	return &Mem{
		root:     root,
		files:    make(map[string][]byte),
		dirs:     map[string]bool{root: true},
		writable: writable,
	}, nil
}

// Root returns the virtual root every logical path is resolved against.
func (m *Mem) Root() string { return m.root }

// resolve turns a caller path into a logical absolute path inside the tree.
func (m *Mem) resolve(p string, sb *Sandbox) (string, error) {
	if p == "" {
		return "", fmt.Errorf("fsx: empty path")
	}
	root := m.root
	cwd := root
	if sb != nil {
		if r := sb.Root(); r != "" {
			root = path.Clean(r)
		}
		if sb.Cwd != "" {
			cwd = path.Clean(sb.Cwd)
		}
	}
	abs := p
	if !path.IsAbs(abs) {
		abs = path.Join(cwd, p)
	}
	abs = path.Clean(abs)
	if !permission.Within(root, abs) {
		return "", denied(abs, "outside "+root)
	}
	return abs, nil
}

func (m *Mem) writablePath(p string, sb *Sandbox) (string, error) {
	abs, err := m.resolve(p, sb)
	if err != nil {
		return "", err
	}
	if !writable(abs, sb, m.writable) {
		return "", denied(abs, "not writable under the policy")
	}
	return abs, nil
}

func notExist(op, p string) error {
	return &fs.PathError{Op: op, Path: p, Err: fs.ErrNotExist}
}

// Canonicalize resolves path and returns its logical absolute form.
func (m *Mem) Canonicalize(_ context.Context, p string, sb *Sandbox) (string, error) {
	return m.resolve(p, sb)
}

// Read returns the whole file.
func (m *Mem) Read(_ context.Context, p string, sb *Sandbox) ([]byte, error) {
	abs, err := m.resolve(p, sb)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.files[abs]
	if !ok {
		return nil, notExist("read", p)
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// ReadStream returns the file as a stream.
func (m *Mem) ReadStream(ctx context.Context, p string, sb *Sandbox) (io.ReadCloser, error) {
	data, err := m.Read(ctx, p, sb)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Write replaces a file, creating missing parents.
func (m *Mem) Write(_ context.Context, p string, data []byte, sb *Sandbox) error {
	abs, err := m.writablePath(p, sb)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.put(abs, data)
	return nil
}

// WriteStream replaces a file from a stream.
func (m *Mem) WriteStream(ctx context.Context, p string, src io.Reader, sb *Sandbox) (int64, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return 0, err
	}
	if err := m.Write(ctx, p, data, sb); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// put stores data and materializes the parent directory chain. The caller holds
// the write lock.
func (m *Mem) put(abs string, data []byte) {
	m.files[abs] = append([]byte(nil), data...)
	for dir := path.Dir(abs); dir != "." && dir != "/"; dir = path.Dir(dir) {
		m.dirs[dir] = true
	}
	m.dirs[m.root] = true
}

// Stat returns file metadata.
func (m *Mem) Stat(_ context.Context, p string, sb *Sandbox) (fs.FileInfo, error) {
	abs, err := m.resolve(p, sb)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if data, ok := m.files[abs]; ok {
		return memInfo{name: path.Base(abs), size: int64(len(data))}, nil
	}
	if m.dirs[abs] {
		return memInfo{name: path.Base(abs), dir: true}, nil
	}
	return nil, notExist("stat", p)
}

// List returns the direct children of dir, sorted by name.
func (m *Mem) List(_ context.Context, dir string, sb *Sandbox) ([]fs.DirEntry, error) {
	abs, err := m.resolve(dir, sb)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.dirs[abs] {
		if _, ok := m.files[abs]; ok {
			return nil, &fs.PathError{Op: "readdir", Path: dir, Err: fs.ErrInvalid}
		}
		return nil, notExist("readdir", dir)
	}
	entries := make(map[string]bool)
	for name := range m.dirs {
		if name != abs && path.Dir(name) == abs {
			entries[path.Base(name)] = true
		}
	}
	for name := range m.files {
		if path.Dir(name) == abs {
			entries[path.Base(name)] = false
		}
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for name, isDir := range entries {
		out = append(out, memEntry{name: name, dir: isDir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Walk visits every entry beneath dir in lexicographic order.
func (m *Mem) Walk(_ context.Context, dir string, sb *Sandbox, fn WalkFunc) error {
	abs, err := m.resolve(dir, sb)
	if err != nil {
		return err
	}
	m.mu.RLock()
	var keys []string
	for name := range m.files {
		keys = append(keys, name)
	}
	for name := range m.dirs {
		if name != abs {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	m.mu.RUnlock()

	prefix := abs + "/"
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rel := strings.TrimPrefix(key, prefix)
		if fn != nil {
			if err := fn(rel, memEntry{name: path.Base(key), dir: m.dirs[key]}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Mkdir creates a directory and its parents.
func (m *Mem) Mkdir(_ context.Context, p string, sb *Sandbox) error {
	abs, err := m.writablePath(p, sb)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for dir := abs; dir != "." && dir != "/"; dir = path.Dir(dir) {
		m.dirs[dir] = true
	}
	return nil
}

// Remove deletes a file, or a directory tree. It is idempotent.
func (m *Mem) Remove(_ context.Context, p string, sb *Sandbox) error {
	abs, err := m.writablePath(p, sb)
	if err != nil {
		return err
	}
	if abs == m.root {
		return fmt.Errorf("fsx: refusing to remove the root")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, abs)
	delete(m.dirs, abs)
	prefix := abs + "/"
	for name := range m.files {
		if strings.HasPrefix(name, prefix) {
			delete(m.files, name)
		}
	}
	for name := range m.dirs {
		if strings.HasPrefix(name, prefix) {
			delete(m.dirs, name)
		}
	}
	return nil
}

// Copy duplicates a file within the tree.
func (m *Mem) Copy(ctx context.Context, src, dst string, sb *Sandbox) error {
	data, err := m.Read(ctx, src, sb)
	if err != nil {
		return err
	}
	return m.Write(ctx, dst, data, sb)
}

type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string { return i.name }
func (i memInfo) Size() int64  { return i.size }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

type memEntry struct {
	name string
	dir  bool
}

func (e memEntry) Name() string               { return e.name }
func (e memEntry) IsDir() bool                { return e.dir }
func (e memEntry) Type() fs.FileMode          { return 0 }
func (e memEntry) Info() (fs.FileInfo, error) { return memInfo{name: e.name, dir: e.dir}, nil }
