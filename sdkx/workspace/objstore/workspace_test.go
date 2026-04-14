package objstore

import (
	"context"
	"testing"
)

func newTestWS() (*MemObjectStore, context.Context) {
	return NewMemObjectStore(), context.Background()
}

func TestWorkspace_ReadWrite(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

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
}

func TestWorkspace_ReadNotFound(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	_, err := ws.Read(ctx, "nope.txt")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestWorkspace_Overwrite(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "f.txt", []byte("v1"))
	ws.Write(ctx, "f.txt", []byte("v2"))

	data, _ := ws.Read(ctx, "f.txt")
	if string(data) != "v2" {
		t.Fatalf("got %q, want 'v2'", data)
	}
}

func TestWorkspace_Delete(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "del.txt", []byte("bye"))
	if err := ws.Delete(ctx, "del.txt"); err != nil {
		t.Fatal(err)
	}
	exists, _ := ws.Exists(ctx, "del.txt")
	if exists {
		t.Fatal("should not exist after delete")
	}
}

func TestWorkspace_RemoveAll(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "dir/a.txt", []byte("a"))
	ws.Write(ctx, "dir/sub/b.txt", []byte("b"))
	ws.Write(ctx, "keep.txt", []byte("keep"))

	if err := ws.RemoveAll(ctx, "dir"); err != nil {
		t.Fatal(err)
	}

	if exists, _ := ws.Exists(ctx, "dir/a.txt"); exists {
		t.Fatal("dir/a.txt should be gone")
	}
	if exists, _ := ws.Exists(ctx, "dir/sub/b.txt"); exists {
		t.Fatal("dir/sub/b.txt should be gone")
	}
	data, _ := ws.Read(ctx, "keep.txt")
	if string(data) != "keep" {
		t.Fatal("keep.txt should be untouched")
	}
}

func TestWorkspace_RemoveAll_Root(t *testing.T) {
	store, _ := newTestWS()
	ws := NewWorkspace(store)

	if err := ws.RemoveAll(context.Background(), "."); err == nil {
		t.Fatal("should refuse to delete root")
	}
}

func TestWorkspace_List(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "a.txt", []byte("a"))
	ws.Write(ctx, "b.txt", []byte("b"))
	ws.Write(ctx, "sub/c.txt", []byte("c"))

	entries, err := ws.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	var files, dirs int
	for _, e := range entries {
		if e.IsDir() {
			dirs++
		} else {
			files++
		}
	}
	if files != 2 {
		t.Fatalf("expected 2 files at root, got %d", files)
	}
	if dirs != 1 {
		t.Fatalf("expected 1 dir at root, got %d", dirs)
	}

	subEntries, err := ws.List(ctx, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(subEntries) != 1 || subEntries[0].Name() != "c.txt" {
		t.Fatalf("sub/ listing: %v", subEntries)
	}
}

func TestWorkspace_ListDirEntryInfo(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "file.txt", []byte("12345"))
	entries, _ := ws.List(ctx, ".")
	if len(entries) != 1 {
		t.Fatalf("expected 1, got %d", len(entries))
	}
	e := entries[0]
	if e.Name() != "file.txt" || e.IsDir() {
		t.Fatalf("entry = %q dir=%v", e.Name(), e.IsDir())
	}
	info, err := e.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 5 {
		t.Fatalf("size = %d", info.Size())
	}
	_ = e.Type()
}

func TestWorkspace_Exists(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "yes.txt", []byte("y"))

	exists, err := ws.Exists(ctx, "yes.txt")
	if err != nil || !exists {
		t.Fatal("should exist")
	}

	exists, err = ws.Exists(ctx, "no.txt")
	if err != nil || exists {
		t.Fatal("should not exist")
	}
}

func TestWorkspace_ExistsImplicitDir(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "dir/file.txt", []byte("x"))

	exists, err := ws.Exists(ctx, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("implicit directory should exist")
	}
}

func TestWorkspace_Stat(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "data.bin", []byte("hello"))
	info, err := ws.Stat(ctx, "data.bin")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name() != "data.bin" {
		t.Fatalf("Name = %q", info.Name())
	}
	if info.Size() != 5 {
		t.Fatalf("Size = %d", info.Size())
	}
	if info.IsDir() {
		t.Fatal("should not be dir")
	}
	if info.Sys() != nil {
		t.Fatal("Sys should be nil")
	}
	if info.Mode() != 0o644 {
		t.Fatalf("Mode = %o", info.Mode())
	}
}

func TestWorkspace_StatImplicitDir(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "mydir/f.txt", []byte("x"))
	info, err := ws.Stat(ctx, "mydir")
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("should be dir")
	}
	if info.Name() != "mydir" {
		t.Fatalf("Name = %q", info.Name())
	}
}

func TestWorkspace_StatNotFound(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	_, err := ws.Stat(ctx, "nope")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestWorkspace_AppendNative(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Append(ctx, "log.txt", []byte("line1\n"))
	ws.Append(ctx, "log.txt", []byte("line2\n"))

	data, _ := ws.Read(ctx, "log.txt")
	if string(data) != "line1\nline2\n" {
		t.Fatalf("got %q", data)
	}
}

func TestWorkspace_AppendFallback(t *testing.T) {
	store, ctx := newTestWS()
	// Wrap in a non-Appender to force the read-modify-write fallback.
	ws := NewWorkspace(&noAppendStore{store})

	ws.Append(ctx, "log.txt", []byte("a"))
	ws.Append(ctx, "log.txt", []byte("b"))

	data, _ := ws.Read(ctx, "log.txt")
	if string(data) != "ab" {
		t.Fatalf("got %q", data)
	}
}

type noAppendStore struct {
	ObjectStore
}

func TestWorkspace_PathTraversal(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	if _, err := ws.Read(ctx, "../etc/passwd"); err == nil {
		t.Fatal("should reject ..")
	}
	if err := ws.Write(ctx, "/absolute", []byte("x")); err == nil {
		t.Fatal("should reject absolute path")
	}
}

func TestWorkspace_WithPrefix(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store, WithPrefix("tenant-42"))

	ws.Write(ctx, "config.json", []byte(`{"a":1}`))

	// Verify the key in the underlying store has the prefix.
	_, err := store.Head(ctx, "tenant-42/config.json")
	if err != nil {
		t.Fatalf("key should be prefixed in store: %v", err)
	}

	// Workspace read should work without knowing the prefix.
	data, err := ws.Read(ctx, "config.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"a":1}` {
		t.Fatalf("got %q", data)
	}
}

func TestWorkspace_WithPrefixIsolation(t *testing.T) {
	store, ctx := newTestWS()
	ws1 := NewWorkspace(store, WithPrefix("ns1"))
	ws2 := NewWorkspace(store, WithPrefix("ns2"))

	ws1.Write(ctx, "secret.txt", []byte("ns1-data"))
	ws2.Write(ctx, "secret.txt", []byte("ns2-data"))

	d1, _ := ws1.Read(ctx, "secret.txt")
	d2, _ := ws2.Read(ctx, "secret.txt")
	if string(d1) != "ns1-data" || string(d2) != "ns2-data" {
		t.Fatalf("d1=%q d2=%q", d1, d2)
	}
}

func TestWorkspace_WithPrefixList(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store, WithPrefix("app"))

	ws.Write(ctx, "a.txt", []byte("a"))
	ws.Write(ctx, "dir/b.txt", []byte("b"))

	entries, err := ws.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestWorkspace_NestedDirs(t *testing.T) {
	store, ctx := newTestWS()
	ws := NewWorkspace(store)

	ws.Write(ctx, "a/b/c/d.txt", []byte("deep"))

	entries, _ := ws.List(ctx, ".")
	if len(entries) != 1 || entries[0].Name() != "a" || !entries[0].IsDir() {
		t.Fatalf("root: %v", entries)
	}

	entries, _ = ws.List(ctx, "a")
	if len(entries) != 1 || entries[0].Name() != "b" {
		t.Fatalf("a/: %v", entries)
	}

	entries, _ = ws.List(ctx, "a/b/c")
	if len(entries) != 1 || entries[0].Name() != "d.txt" {
		t.Fatalf("a/b/c/: %v", entries)
	}
}
