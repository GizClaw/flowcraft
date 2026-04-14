package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdkx/workspace/objstore"
)

// mockClient implements Client using an in-memory map, proving the
// S3 Store correctly delegates to the Client interface.
type mockClient struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func newMockClient() *mockClient {
	return &mockClient{data: make(map[string][]byte)}
}

func (m *mockClient) GetObject(_ context.Context, in *GetObjectInput) (*GetObjectOutput, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.data[in.Key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", in.Key)
	}
	return &GetObjectOutput{Body: io.NopCloser(bytes.NewReader(d))}, nil
}

func (m *mockClient) PutObject(_ context.Context, in *PutObjectInput) error {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[in.Key] = data
	return nil
}

func (m *mockClient) DeleteObject(_ context.Context, in *DeleteObjectInput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, in.Key)
	return nil
}

func (m *mockClient) HeadObject(_ context.Context, in *HeadObjectInput) (*HeadObjectOutput, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.data[in.Key]
	if !ok {
		return nil, fmt.Errorf("NotFound: %s", in.Key)
	}
	return &HeadObjectOutput{
		ObjectInfo: objstore.ObjectInfo{
			Key:          in.Key,
			Size:         int64(len(d)),
			LastModified: time.Now(),
		},
	}, nil
}

func (m *mockClient) ListObjectsV2(_ context.Context, in *ListObjectsV2Input) (*ListObjectsV2Output, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := &ListObjectsV2Output{}
	cpSet := make(map[string]bool)

	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if !strings.HasPrefix(k, in.Prefix) {
			continue
		}
		rest := k[len(in.Prefix):]
		if in.Delimiter != "" {
			if idx := strings.Index(rest, in.Delimiter); idx >= 0 {
				cp := in.Prefix + rest[:idx+len(in.Delimiter)]
				if !cpSet[cp] {
					cpSet[cp] = true
					out.CommonPrefixes = append(out.CommonPrefixes, cp)
				}
				continue
			}
		}
		out.Contents = append(out.Contents, objstore.ObjectInfo{
			Key:  k,
			Size: int64(len(m.data[k])),
		})
	}
	return out, nil
}

func (m *mockClient) DeleteObjects(_ context.Context, in *DeleteObjectsInput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range in.Keys {
		delete(m.data, k)
	}
	return nil
}

var _ Client = (*mockClient)(nil)

func TestS3Store_CRUD(t *testing.T) {
	store := New(newMockClient(), "test-bucket")
	ctx := context.Background()

	if err := store.Put(ctx, "key1", []byte("value1")); err != nil {
		t.Fatal(err)
	}
	data, err := store.Get(ctx, "key1")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value1" {
		t.Fatalf("got %q", data)
	}

	info, err := store.Head(ctx, "key1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 6 {
		t.Fatalf("size = %d", info.Size)
	}

	if err := store.Del(ctx, "key1"); err != nil {
		t.Fatal(err)
	}
	_, err = store.Get(ctx, "key1")
	if err == nil {
		t.Fatal("expected not-found after delete")
	}
}

func TestS3Store_ListPrefix(t *testing.T) {
	client := newMockClient()
	store := New(client, "b")
	ctx := context.Background()

	store.Put(ctx, "dir/a.txt", []byte("a"))
	store.Put(ctx, "dir/b.txt", []byte("b"))
	store.Put(ctx, "dir/sub/c.txt", []byte("c"))
	store.Put(ctx, "other.txt", []byte("o"))

	result, err := store.ListPrefix(ctx, "dir/", "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Objects) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(result.Objects))
	}
	if len(result.CommonPrefixes) != 1 || result.CommonPrefixes[0] != "dir/sub/" {
		t.Fatalf("CommonPrefixes = %v", result.CommonPrefixes)
	}
}

func TestS3Store_DelPrefix(t *testing.T) {
	client := newMockClient()
	store := New(client, "b")
	ctx := context.Background()

	store.Put(ctx, "dir/a.txt", []byte("a"))
	store.Put(ctx, "dir/sub/b.txt", []byte("b"))
	store.Put(ctx, "keep.txt", []byte("k"))

	if err := store.DelPrefix(ctx, "dir/"); err != nil {
		t.Fatal(err)
	}

	_, err := store.Get(ctx, "dir/a.txt")
	if err == nil {
		t.Fatal("dir/a.txt should be deleted")
	}
	data, _ := store.Get(ctx, "keep.txt")
	if string(data) != "k" {
		t.Fatal("keep.txt should remain")
	}
}

func TestS3Store_GetNotFound(t *testing.T) {
	store := New(newMockClient(), "b")
	_, err := store.Get(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestS3Store_HeadNotFound(t *testing.T) {
	store := New(newMockClient(), "b")
	_, err := store.Head(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestS3Store_AsWorkspace(t *testing.T) {
	store := New(newMockClient(), "test-bucket")
	ws := objstore.NewWorkspace(store, objstore.WithPrefix("ws"))
	ctx := context.Background()

	if err := ws.Write(ctx, "hello.txt", []byte("world")); err != nil {
		t.Fatal(err)
	}
	data, err := ws.Read(ctx, "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "world" {
		t.Fatalf("got %q", data)
	}

	entries, err := ws.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}
