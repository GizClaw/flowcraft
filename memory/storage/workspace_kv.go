package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// WorkspaceKV implements Store on one workspace. Each key maps to an encoded
// path under kvRoot; every segment is encoded so no user input reaches a
// filesystem path verbatim. A key that is a prefix of another key cannot be
// represented (the parent would be both a file and a directory) and is
// rejected on write.
//
// Check-and-write operations (PutIfAbsent, CompareAndSwap) are atomic only
// within one process: the adapter serializes writers with a mutex.
type WorkspaceKV struct {
	ws workspace.Workspace
	mu sync.Mutex
}

var _ Store = (*WorkspaceKV)(nil)
var _ CASStore = (*WorkspaceKV)(nil)
var _ PutIfAbsentStore = (*WorkspaceKV)(nil)

// NewWorkspaceKV constructs a workspace-backed key-value store.
func NewWorkspaceKV(ws workspace.Workspace) (*WorkspaceKV, error) {
	if nilWorkspace(ws) {
		return nil, errors.New("storage: workspace is required")
	}
	return &WorkspaceKV{ws: ws}, nil
}

// Get implements Store.
func (store *WorkspaceKV) Get(ctx context.Context, key string) ([]byte, error) {
	target, err := store.keyPath(key)
	if err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	data, err := store.ws.Read(ctx, target)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage kv: read %q: %w", key, err)
	}
	return data, nil
}

// Put implements Store.
func (store *WorkspaceKV) Put(ctx context.Context, key string, data []byte) error {
	target, err := store.keyPath(key)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkLayout(ctx, target, key); err != nil {
		return err
	}
	if err := workspace.AtomicWrite(ctx, store.ws, target, data); err != nil {
		return fmt.Errorf("storage kv: put %q: %w", key, err)
	}
	return nil
}

// Delete implements Store.
func (store *WorkspaceKV) Delete(ctx context.Context, key string) error {
	target, err := store.keyPath(key)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.ws.Delete(ctx, target); err != nil && !isNotFound(err) {
		return fmt.Errorf("storage kv: delete %q: %w", key, err)
	}
	return nil
}

// List implements Store.
func (store *WorkspaceKV) List(ctx context.Context, prefix string) ([]Entry, error) {
	if prefix != "" {
		if err := validateName(prefix); err != nil {
			return nil, err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	base := prefixBase(prefix)
	basePath := kvRoot
	if base != "" {
		var err error
		basePath, err = nameToPath(kvRoot, base)
		if err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0)
	err := workspace.Walk(ctx, store.ws, basePath, func(child string, entry fs.DirEntry) error {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		name, err := pathToName(kvRoot, child)
		if err != nil {
			return err
		}
		if nameHasPrefix(name, prefix) {
			keys = append(keys, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	entries := make([]Entry, 0, len(keys))
	for _, key := range keys {
		data, err := store.ws.Read(ctx, path.Join(basePathForName(kvRoot, key)))
		if err != nil {
			return nil, fmt.Errorf("storage kv: read %q during list: %w", key, err)
		}
		entries = append(entries, Entry{Key: key, Value: data})
	}
	return entries, nil
}

// CompareAndSwap implements CASStore.
func (store *WorkspaceKV) CompareAndSwap(ctx context.Context, key string, old, new []byte) (bool, error) {
	target, err := store.keyPath(key)
	if err != nil {
		return false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkLayout(ctx, target, key); err != nil {
		return false, err
	}
	current, err := store.ws.Read(ctx, target)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("storage kv: cas read %q: %w", key, err)
	}
	if !bytes.Equal(current, old) {
		return false, nil
	}
	if err := workspace.AtomicWrite(ctx, store.ws, target, new); err != nil {
		return false, fmt.Errorf("storage kv: cas write %q: %w", key, err)
	}
	return true, nil
}

// PutIfAbsent implements PutIfAbsentStore.
func (store *WorkspaceKV) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	target, err := store.keyPath(key)
	if err != nil {
		return false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkLayout(ctx, target, key); err != nil {
		return false, err
	}
	if _, err := store.ws.Read(ctx, target); err == nil {
		return false, nil
	} else if !isNotFound(err) {
		return false, fmt.Errorf("storage kv: put-if-absent read %q: %w", key, err)
	}
	if err := workspace.AtomicWrite(ctx, store.ws, target, data); err != nil {
		return false, fmt.Errorf("storage kv: put-if-absent write %q: %w", key, err)
	}
	return true, nil
}

func (store *WorkspaceKV) keyPath(key string) (string, error) {
	if err := validateName(key); err != nil {
		return "", err
	}
	return nameToPath(kvRoot, key)
}

// checkLayout rejects keys that the hierarchical workspace layout cannot
// represent: a key under an existing value (file/directory conflict) or a
// key that already exists as a directory.
func (store *WorkspaceKV) checkLayout(ctx context.Context, target, key string) error {
	parent := path.Dir(target)
	for parent != kvRoot {
		info, err := store.ws.Stat(ctx, parent)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%w: key %q has an ancestor value", ErrConflict, key)
			}
		} else if !isNotFound(err) {
			return err
		}
		parent = path.Dir(parent)
	}
	info, err := store.ws.Stat(ctx, target)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("%w: key %q is a directory", ErrConflict, key)
		}
	} else if !isNotFound(err) {
		return err
	}
	return nil
}

// basePathForName mirrors nameToPath without re-validating (list has already
// decoded canonical names and the workspace tree exists).
func basePathForName(root, name string) string {
	segments := strings.Split(name, "/")
	encoded := make([]string, len(segments))
	for index, segment := range segments {
		encoded[index] = encodeSegment(segment)
	}
	return strings.Join(append([]string{root}, encoded...), "/")
}
