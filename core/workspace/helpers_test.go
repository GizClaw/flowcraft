package workspace

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestRename_Mem(t *testing.T) {
	ws := newTestWorkspace(t)
	ctx := context.Background()
	mustWrite(t, ws, "a/old.txt", []byte("content"))
	if err := ws.Rename(ctx, "a/old.txt", "b/new.txt"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := ws.Exists(ctx, "a/old.txt"); exists {
		t.Fatal("old file should not exist after rename")
	}
	data, err := ws.Read(ctx, "b/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Fatalf("got %q, want 'content'", data)
	}
}

func TestRename_SrcNotFound_Mem(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.Rename(context.Background(), "nope.txt", "dst.txt"); err == nil {
		t.Fatal("expected error when src does not exist")
	}
}

func TestAtomicWrite(t *testing.T) {
	ws := newTestWorkspace(t)
	ctx := context.Background()
	if err := AtomicWrite(ctx, ws, "out/x.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := ws.Read(ctx, "out/x.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("read: data=%q err=%v", data, err)
	}
	// tmp file must be cleaned up.
	if exists, _ := ws.Exists(ctx, "out/.x.txt.tmp"); exists {
		t.Fatal("tmp file should not exist after AtomicWrite")
	}
}

func TestCopy(t *testing.T) {
	ws := newTestWorkspace(t)
	ctx := context.Background()

	mustWrite(t, ws, "src.txt", []byte("original"))
	if err := Copy(ctx, ws, "src.txt", "dst.txt"); err != nil {
		t.Fatal(err)
	}

	srcData, _ := ws.Read(ctx, "src.txt")
	dstData, _ := ws.Read(ctx, "dst.txt")
	if string(srcData) != "original" || string(dstData) != "original" {
		t.Fatalf("src=%q dst=%q", srcData, dstData)
	}
}

func TestCopy_SrcNotFound(t *testing.T) {
	ws := newTestWorkspace(t)
	err := Copy(context.Background(), ws, "nope.txt", "dst.txt")
	if err == nil {
		t.Fatal("expected error when src does not exist")
	}
}

func TestWalk(t *testing.T) {
	ws := newTestWorkspace(t)
	mustWrite(t, ws, "a.txt", []byte("a"))
	mustWrite(t, ws, "sub/b.txt", []byte("b"))
	mustWrite(t, ws, "sub/deep/c.txt", []byte("c"))

	var files []string
	err := Walk(context.Background(), ws, ".", func(path string, entry fs.DirEntry) error {
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(files), files)
	}
}

func TestWalk_SkipDir(t *testing.T) {
	ws := newTestWorkspace(t)
	mustWrite(t, ws, "a/1.txt", []byte("1"))
	mustWrite(t, ws, "skip/2.txt", []byte("2"))
	mustWrite(t, ws, "b/3.txt", []byte("3"))

	var visited []string
	err := Walk(context.Background(), ws, ".", func(path string, entry fs.DirEntry) error {
		if entry.IsDir() && entry.Name() == "skip" {
			return filepath.SkipDir
		}
		visited = append(visited, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range visited {
		if p == "skip/2.txt" || p == "skip" {
			t.Fatalf("should have skipped %q", p)
		}
	}
}

func TestWalk_Error(t *testing.T) {
	ws := newTestWorkspace(t)
	mustWrite(t, ws, "a.txt", []byte("a"))

	sentinel := errors.New("stop")
	err := Walk(context.Background(), ws, ".", func(_ string, _ fs.DirEntry) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestWalk_MissingRoot(t *testing.T) {
	ws := newTestWorkspace(t)
	called := false
	err := Walk(context.Background(), ws, "missing", func(_ string, _ fs.DirEntry) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking a missing directory should not error: %v", err)
	}
	if called {
		t.Fatal("walk over a missing directory should not visit entries")
	}
}

func TestGlob_Doublestar(t *testing.T) {
	ws := newTestWorkspace(t)
	mustWrite(t, ws, "readme.md", []byte("r"))
	mustWrite(t, ws, "src/main.go", []byte("m"))
	mustWrite(t, ws, "src/util.go", []byte("u"))
	mustWrite(t, ws, "src/sub/helper.go", []byte("h"))
	mustWrite(t, ws, "docs/index.html", []byte("i"))

	ctx := context.Background()

	matches, err := Glob(ctx, ws, "**/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 3 {
		t.Fatalf("expected 3 .go files, got %d: %v", len(matches), matches)
	}
}

func TestGlob_SingleLevel(t *testing.T) {
	ws := newTestWorkspace(t)
	mustWrite(t, ws, "src/main.go", []byte("m"))
	mustWrite(t, ws, "src/util.go", []byte("u"))
	mustWrite(t, ws, "src/sub/helper.go", []byte("h"))

	matches, err := Glob(context.Background(), ws, "src/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2, got %d: %v", len(matches), matches)
	}
}

func TestGlob_NoMatch(t *testing.T) {
	ws := newTestWorkspace(t)
	mustWrite(t, ws, "a.txt", []byte("a"))

	matches, err := Glob(context.Background(), ws, "**/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestGlob_ExactFile(t *testing.T) {
	ws := newTestWorkspace(t)
	mustWrite(t, ws, "config.yaml", []byte("x"))
	mustWrite(t, ws, "other.txt", []byte("y"))

	matches, err := Glob(context.Background(), ws, "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != "config.yaml" {
		t.Fatalf("got %v", matches)
	}
}
