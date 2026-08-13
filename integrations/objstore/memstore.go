package objstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemObjectStore is an in-memory ObjectStore for testing.
// It also implements Appender for testing the native-append path.
type MemObjectStore struct {
	mu      sync.RWMutex
	objects map[string]*memObject
}

type memObject struct {
	data         []byte
	lastModified time.Time
}

func NewMemObjectStore() *MemObjectStore {
	return &MemObjectStore{objects: make(map[string]*memObject)}
}

func (m *MemObjectStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, key)
	}
	cp := make([]byte, len(obj.data))
	copy(cp, obj.data)
	return cp, nil
}

func (m *MemObjectStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.objects[key] = &memObject{data: cp, lastModified: time.Now()}
	return nil
}

func (m *MemObjectStore) Del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *MemObjectStore) Head(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	obj, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, fmt.Errorf("%w: %s", ErrKeyNotFound, key)
	}
	return ObjectInfo{
		Key:          key,
		Size:         int64(len(obj.data)),
		LastModified: obj.lastModified,
	}, nil
}

func (m *MemObjectStore) ListPrefix(_ context.Context, prefix, delimiter string) (*ListResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := &ListResult{}
	commonPrefixSet := make(map[string]bool)

	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if delimiter != "" {
			if idx := strings.Index(rest, delimiter); idx >= 0 {
				cp := prefix + rest[:idx+len(delimiter)]
				if !commonPrefixSet[cp] {
					commonPrefixSet[cp] = true
					result.CommonPrefixes = append(result.CommonPrefixes, cp)
				}
				continue
			}
		}
		obj := m.objects[k]
		result.Objects = append(result.Objects, ObjectInfo{
			Key:          k,
			Size:         int64(len(obj.data)),
			LastModified: obj.lastModified,
		})
	}
	return result, nil
}

func (m *MemObjectStore) DelPrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			delete(m.objects, k)
		}
	}
	return nil
}

// Append implements Appender for testing the native-append code path.
func (m *MemObjectStore) Append(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		m.objects[key] = &memObject{data: cp, lastModified: time.Now()}
		return nil
	}
	obj.data = append(obj.data, data...)
	obj.lastModified = time.Now()
	return nil
}

var (
	_ ObjectStore = (*MemObjectStore)(nil)
	_ Appender    = (*MemObjectStore)(nil)
)
