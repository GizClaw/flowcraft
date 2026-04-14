package workspace

import (
	"context"
	"testing"
)

func TestMemWorkspace_CRUD(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	ws.MustWrite("a.txt", []byte("alpha"))
	ws.MustWrite("b.txt", []byte("beta"))

	data, err := ws.Read(ctx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alpha" {
		t.Fatalf("got %q, want 'alpha'", data)
	}

	if err := ws.Append(ctx, "a.txt", []byte(" world")); err != nil {
		t.Fatal(err)
	}
	data, _ = ws.Read(ctx, "a.txt")
	if string(data) != "alpha world" {
		t.Fatalf("got %q, want 'alpha world'", data)
	}

	entries, err := ws.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestMemWorkspace_ReadNotFound(t *testing.T) {
	ws := NewMemWorkspace()
	_, err := ws.Read(context.Background(), "nope.txt")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestMemWorkspace_ReadDir(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("dir/file.txt", []byte("x"))

	_, err := ws.Read(context.Background(), "dir")
	if err == nil {
		t.Fatal("expected error reading a directory")
	}
}

func TestMemWorkspace_AppendCreatesNew(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	if err := ws.Append(ctx, "new.txt", []byte("first")); err != nil {
		t.Fatal(err)
	}
	data, _ := ws.Read(ctx, "new.txt")
	if string(data) != "first" {
		t.Fatalf("got %q", data)
	}
}

func TestMemWorkspace_AppendToDir(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("dir/file.txt", []byte("x"))

	err := ws.Append(context.Background(), "dir", []byte("bad"))
	if err == nil {
		t.Fatal("expected error appending to a directory")
	}
}

func TestMemWorkspace_DeleteDirectory(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	ws.MustWrite("dir/file.txt", []byte("data"))

	if err := ws.Delete(ctx, "dir"); err == nil {
		t.Fatal("expected Delete on directory to fail")
	}

	data, err := ws.Read(ctx, "dir/file.txt")
	if err != nil {
		t.Fatalf("child file should still exist: %v", err)
	}
	if string(data) != "data" {
		t.Fatalf("got %q, want 'data'", data)
	}
}

func TestMemWorkspace_DeleteNotFound(t *testing.T) {
	ws := NewMemWorkspace()
	err := ws.Delete(context.Background(), "nope.txt")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestMemWorkspace_RemoveAll(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	ws.MustWrite("dir/a.txt", []byte("a"))
	ws.MustWrite("dir/sub/b.txt", []byte("b"))
	ws.MustWrite("other.txt", []byte("keep"))

	if err := ws.RemoveAll(ctx, "dir"); err != nil {
		t.Fatal(err)
	}

	if exists, _ := ws.Exists(ctx, "dir/a.txt"); exists {
		t.Fatal("dir/a.txt should be gone")
	}
	if exists, _ := ws.Exists(ctx, "dir/sub/b.txt"); exists {
		t.Fatal("dir/sub/b.txt should be gone")
	}

	data, _ := ws.Read(ctx, "other.txt")
	if string(data) != "keep" {
		t.Fatal("other.txt should be untouched")
	}
}

func TestMemWorkspace_RemoveAll_Root(t *testing.T) {
	ws := NewMemWorkspace()
	err := ws.RemoveAll(context.Background(), ".")
	if err == nil {
		t.Fatal("should refuse to remove root")
	}
}

func TestMemWorkspace_Stat(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("data.txt", []byte("12345"))

	info, err := ws.Stat(context.Background(), "data.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name() != "data.txt" {
		t.Fatalf("Name = %q", info.Name())
	}
	if info.Size() != 5 {
		t.Fatalf("Size = %d, want 5", info.Size())
	}
	if info.IsDir() {
		t.Fatal("should not be dir")
	}
	if info.Sys() != nil {
		t.Fatal("Sys() should be nil")
	}
	if info.ModTime().IsZero() {
		t.Fatal("ModTime should not be zero")
	}
	mode := info.Mode()
	if mode != 0o644 {
		t.Fatalf("Mode = %o, want 644", mode)
	}
}

func TestMemWorkspace_StatImplicitDir(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("dir/file.txt", []byte("x"))

	info, err := ws.Stat(context.Background(), "dir")
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
	if info.Name() != "dir" {
		t.Fatalf("Name = %q, want 'dir'", info.Name())
	}
}

func TestMemWorkspace_StatNotFound(t *testing.T) {
	ws := NewMemWorkspace()
	_, err := ws.Stat(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestMemWorkspace_ExistsImplicitDir(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("dir/file.txt", []byte("x"))

	exists, err := ws.Exists(context.Background(), "dir")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("implicit directory should exist")
	}
}

func TestMemWorkspace_ExistsNotFound(t *testing.T) {
	ws := NewMemWorkspace()
	exists, err := ws.Exists(context.Background(), "nope")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("should not exist")
	}
}

func TestMemWorkspace_Contains(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("note.txt", []byte("hello world"))

	if !ws.Contains("note.txt", "world") {
		t.Fatal("should contain 'world'")
	}
	if ws.Contains("note.txt", "missing") {
		t.Fatal("should not contain 'missing'")
	}
	if ws.Contains("nope.txt", "x") {
		t.Fatal("nonexistent file should return false")
	}
}

func TestMemWorkspace_ListNested(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	ws.MustWrite("a/1.txt", []byte("1"))
	ws.MustWrite("a/2.txt", []byte("2"))
	ws.MustWrite("b/3.txt", []byte("3"))

	entries, err := ws.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("root should have 2 dirs, got %d", len(entries))
	}
	for _, e := range entries {
		if !e.IsDir() {
			t.Fatalf("%q should be a directory", e.Name())
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		_ = info
		_ = e.Type()
	}

	entriesA, err := ws.List(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	var fileCount int
	for _, e := range entriesA {
		if !e.IsDir() {
			fileCount++
		}
	}
	if fileCount != 2 {
		t.Fatalf("a/ should have 2 files, got %d (total entries %d)", fileCount, len(entriesA))
	}
}

func TestMemWorkspace_CleanPathTraversal(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	_, err := ws.Read(ctx, "../etc/passwd")
	if err == nil {
		t.Fatal("expected path traversal error")
	}

	err = ws.Write(ctx, "/absolute", []byte("x"))
	if err == nil {
		t.Fatal("expected absolute path error")
	}
}

func TestMemWorkspace_WriteOverwrite(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	ws.MustWrite("f.txt", []byte("v1"))
	ws.MustWrite("f.txt", []byte("v2"))

	data, _ := ws.Read(ctx, "f.txt")
	if string(data) != "v2" {
		t.Fatalf("got %q, want 'v2'", data)
	}
}

func TestMemWorkspace_DirEntryInfo(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("file.txt", []byte("hello"))

	entries, _ := ws.List(context.Background(), ".")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]

	if e.Name() != "file.txt" {
		t.Fatalf("Name = %q", e.Name())
	}
	if e.IsDir() {
		t.Fatal("should not be dir")
	}

	info, err := e.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 5 {
		t.Fatalf("Size = %d, want 5", info.Size())
	}

	mode := e.Type()
	if mode != 0 {
		t.Fatalf("Type = %v, want 0 for regular file", mode)
	}
}
