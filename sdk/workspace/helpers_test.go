package workspace

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestRename(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	ws.MustWrite("old.txt", []byte("content"))
	if err := Rename(ctx, ws, "old.txt", "new.txt"); err != nil {
		t.Fatal(err)
	}

	if exists, _ := ws.Exists(ctx, "old.txt"); exists {
		t.Fatal("old file should not exist after rename")
	}
	data, err := ws.Read(ctx, "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Fatalf("got %q, want 'content'", data)
	}
}

func TestRename_SrcNotFound(t *testing.T) {
	ws := NewMemWorkspace()
	err := Rename(context.Background(), ws, "nope.txt", "dst.txt")
	if err == nil {
		t.Fatal("expected error when src does not exist")
	}
}

func TestCopy(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := context.Background()

	ws.MustWrite("src.txt", []byte("original"))
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
	ws := NewMemWorkspace()
	err := Copy(context.Background(), ws, "nope.txt", "dst.txt")
	if err == nil {
		t.Fatal("expected error when src does not exist")
	}
}

func TestWalk(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("a.txt", []byte("a"))
	ws.MustWrite("sub/b.txt", []byte("b"))
	ws.MustWrite("sub/deep/c.txt", []byte("c"))

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
	ws := NewMemWorkspace()
	ws.MustWrite("a/1.txt", []byte("1"))
	ws.MustWrite("skip/2.txt", []byte("2"))
	ws.MustWrite("b/3.txt", []byte("3"))

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
	ws := NewMemWorkspace()
	ws.MustWrite("a.txt", []byte("a"))

	sentinel := errors.New("stop")
	err := Walk(context.Background(), ws, ".", func(_ string, _ fs.DirEntry) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestGlob_Doublestar(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("readme.md", []byte("r"))
	ws.MustWrite("src/main.go", []byte("m"))
	ws.MustWrite("src/util.go", []byte("u"))
	ws.MustWrite("src/sub/helper.go", []byte("h"))
	ws.MustWrite("docs/index.html", []byte("i"))

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
	ws := NewMemWorkspace()
	ws.MustWrite("src/main.go", []byte("m"))
	ws.MustWrite("src/util.go", []byte("u"))
	ws.MustWrite("src/sub/helper.go", []byte("h"))

	matches, err := Glob(context.Background(), ws, "src/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2, got %d: %v", len(matches), matches)
	}
}

func TestGlob_NoMatch(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("a.txt", []byte("a"))

	matches, err := Glob(context.Background(), ws, "**/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestGlob_ExactFile(t *testing.T) {
	ws := NewMemWorkspace()
	ws.MustWrite("config.yaml", []byte("x"))
	ws.MustWrite("other.txt", []byte("y"))

	matches, err := Glob(context.Background(), ws, "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != "config.yaml" {
		t.Fatalf("got %v", matches)
	}
}
