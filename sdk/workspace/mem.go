package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// MemWorkspace implements Workspace entirely in memory.
type MemWorkspace struct {
	mu    sync.RWMutex
	files map[string]*memFile
}

type memFile struct {
	data    []byte
	modTime time.Time
	isDir   bool
}

func NewMemWorkspace() *MemWorkspace {
	return &MemWorkspace{files: make(map[string]*memFile)}
}

func (m *MemWorkspace) Read(_ context.Context, path string) ([]byte, error) {
	p, err := cleanPath(path)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.files[p]
	if !ok || f.isDir {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	cp := make([]byte, len(f.data))
	copy(cp, f.data)
	return cp, nil
}

func (m *MemWorkspace) Write(_ context.Context, path string, data []byte) error {
	p, err := cleanPath(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureParents(p)
	cp := make([]byte, len(data))
	copy(cp, data)
	m.files[p] = &memFile{data: cp, modTime: time.Now()}
	return nil
}

func (m *MemWorkspace) Append(_ context.Context, path string, data []byte) error {
	p, err := cleanPath(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureParents(p)
	f, ok := m.files[p]
	if !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		m.files[p] = &memFile{data: cp, modTime: time.Now()}
		return nil
	}
	if f.isDir {
		return fmt.Errorf("workspace: %s is a directory", path)
	}
	f.data = append(f.data, data...)
	f.modTime = time.Now()
	return nil
}

func (m *MemWorkspace) Rename(_ context.Context, src, dst string) error {
	srcP, err := cleanPath(src)
	if err != nil {
		return err
	}
	dstP, err := cleanPath(dst)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[srcP]
	if !ok || f.isDir {
		return fmt.Errorf("%w: %s", ErrNotFound, src)
	}
	if srcP == dstP {
		return nil
	}
	m.ensureParents(dstP)
	m.files[dstP] = f
	delete(m.files, srcP)
	f.modTime = time.Now()
	return nil
}

func (m *MemWorkspace) Delete(_ context.Context, path string) error {
	p, err := cleanPath(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[p]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if f.isDir {
		return fmt.Errorf("workspace: %s is a directory (use RemoveAll)", path)
	}
	prefix := p + "/"
	for k := range m.files {
		if strings.HasPrefix(k, prefix) {
			return fmt.Errorf("workspace: %s is a directory (use RemoveAll)", path)
		}
	}
	delete(m.files, p)
	return nil
}

func (m *MemWorkspace) RemoveAll(_ context.Context, path string) error {
	p, err := cleanPath(path)
	if err != nil {
		return err
	}
	if p == "" {
		return errdefs.Forbiddenf("workspace: refusing to remove root")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := p + "/"
	for k := range m.files {
		if k == p || strings.HasPrefix(k, prefix) {
			delete(m.files, k)
		}
	}
	return nil
}

func (m *MemWorkspace) List(_ context.Context, dir string) ([]fs.DirEntry, error) {
	p, err := cleanPath(dir)
	if err != nil {
		return nil, err
	}
	prefix := p
	if prefix != "" {
		prefix += "/"
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool)
	var entries []fs.DirEntry
	for k, f := range m.files {
		if !strings.HasPrefix(k, prefix) && k != p {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if rest == "" {
			continue
		}
		parts := strings.SplitN(rest, "/", 2)
		name := parts[0]
		if seen[name] {
			continue
		}
		seen[name] = true
		if len(parts) > 1 {
			entries = append(entries, &memDirEntry{name: name, dir: true, modTime: f.modTime})
		} else {
			entries = append(entries, &memDirEntry{name: name, dir: f.isDir, size: int64(len(f.data)), modTime: f.modTime})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func (m *MemWorkspace) Exists(_ context.Context, path string) (bool, error) {
	p, err := cleanPath(path)
	if err != nil {
		return false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.files[p]; ok {
		return true, nil
	}
	prefix := p + "/"
	for k := range m.files {
		if strings.HasPrefix(k, prefix) {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemWorkspace) Stat(_ context.Context, path string) (fs.FileInfo, error) {
	p, err := cleanPath(path)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.files[p]
	if ok {
		return &memFileInfo{name: filepath.Base(p), size: int64(len(f.data)), modTime: f.modTime, dir: f.isDir}, nil
	}
	prefix := p + "/"
	for k := range m.files {
		if strings.HasPrefix(k, prefix) {
			return &memFileInfo{name: filepath.Base(p), dir: true, modTime: time.Now()}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
}

// MustWrite panics on error — intended for tests.
func (m *MemWorkspace) MustWrite(path string, data []byte) {
	if err := m.Write(context.Background(), path, data); err != nil {
		panic(err)
	}
}

// Contains checks if a path contains a substring — intended for tests.
func (m *MemWorkspace) Contains(path, substr string) bool {
	data, err := m.Read(context.Background(), path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(substr))
}

func (m *MemWorkspace) ensureParents(path string) {
	dir := filepath.Dir(path)
	for dir != "." && dir != "" {
		if _, ok := m.files[dir]; !ok {
			m.files[dir] = &memFile{isDir: true, modTime: time.Now()}
		}
		dir = filepath.Dir(dir)
	}
}

func cleanPath(path string) (string, error) {
	if path == "" || path == "." {
		return "", nil
	}
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, path)
	}
	return cleaned, nil
}

type memDirEntry struct {
	name    string
	dir     bool
	size    int64
	modTime time.Time
}

func (e *memDirEntry) Name() string               { return e.name }
func (e *memDirEntry) IsDir() bool                { return e.dir }
func (e *memDirEntry) Type() fs.FileMode          { return e.info().Mode().Type() }
func (e *memDirEntry) Info() (fs.FileInfo, error) { return e.info(), nil }
func (e *memDirEntry) info() *memFileInfo {
	return &memFileInfo{name: e.name, size: e.size, modTime: e.modTime, dir: e.dir}
}

type memFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	dir     bool
}

func (i *memFileInfo) Name() string       { return i.name }
func (i *memFileInfo) Size() int64        { return i.size }
func (i *memFileInfo) ModTime() time.Time { return i.modTime }
func (i *memFileInfo) IsDir() bool        { return i.dir }
func (i *memFileInfo) Sys() any           { return nil }
func (i *memFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
