package workspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalWorkspace_ReadWrite(t *testing.T) {
	ws, ctx := newLocalWS(t)

	if err := ws.Write(ctx, "test.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}

	data, err := ws.Read(ctx, "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q, want 'hello'", data)
	}

	exists, err := ws.Exists(ctx, "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected file to exist")
	}

	if err := ws.Delete(ctx, "test.txt"); err != nil {
		t.Fatal(err)
	}
	exists, _ = ws.Exists(ctx, "test.txt")
	if exists {
		t.Fatal("expected file to be deleted")
	}
}

func TestLocalWorkspace_Root(t *testing.T) {
	ws, _ := newLocalWS(t)
	root := ws.Root()
	if root == "" {
		t.Fatal("Root() returned empty string")
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("Root() should be absolute, got %q", root)
	}
}

func TestLocalWorkspace_NestedDir(t *testing.T) {
	ws, ctx := newLocalWS(t)

	nested := filepath.Join("sub", "dir", "file.txt")
	if err := ws.Write(ctx, nested, []byte("nested")); err != nil {
		t.Fatal(err)
	}

	data, err := ws.Read(ctx, nested)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "nested" {
		t.Fatalf("got %q, want 'nested'", data)
	}
}

func TestLocalWorkspace_Append(t *testing.T) {
	ws, ctx := newLocalWS(t)

	if err := ws.Append(ctx, "log.txt", []byte("line1\n")); err != nil {
		t.Fatal(err)
	}
	if err := ws.Append(ctx, "log.txt", []byte("line2\n")); err != nil {
		t.Fatal(err)
	}

	data, err := ws.Read(ctx, "log.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "line1\nline2\n" {
		t.Fatalf("got %q", data)
	}
}

func TestLocalWorkspace_List(t *testing.T) {
	ws, ctx := newLocalWS(t)

	ws.Write(ctx, "a.txt", []byte("a"))
	ws.Write(ctx, "b.txt", []byte("b"))
	ws.Write(ctx, "sub/c.txt", []byte("c"))

	entries, err := ws.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	subEntries, err := ws.List(ctx, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(subEntries) != 1 {
		t.Fatalf("expected 1 entry in sub/, got %d", len(subEntries))
	}
}

func TestLocalWorkspace_ListNotFound(t *testing.T) {
	ws, ctx := newLocalWS(t)

	_, err := ws.List(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error listing nonexistent dir")
	}
}

func TestLocalWorkspace_Stat(t *testing.T) {
	ws, ctx := newLocalWS(t)

	ws.Write(ctx, "data.txt", []byte("12345"))
	info, err := ws.Stat(ctx, "data.txt")
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
		t.Fatal("expected file, not dir")
	}
}

func TestLocalWorkspace_StatNotFound(t *testing.T) {
	ws, ctx := newLocalWS(t)

	_, err := ws.Stat(ctx, "nope.txt")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLocalWorkspace_RemoveAll(t *testing.T) {
	ws, ctx := newLocalWS(t)

	ws.Write(ctx, "dir/a.txt", []byte("a"))
	ws.Write(ctx, "dir/sub/b.txt", []byte("b"))

	if err := ws.RemoveAll(ctx, "dir"); err != nil {
		t.Fatal(err)
	}

	exists, _ := ws.Exists(ctx, "dir")
	if exists {
		t.Fatal("dir should be gone")
	}
}

func TestLocalWorkspace_RemoveAll_Root(t *testing.T) {
	ws, ctx := newLocalWS(t)

	err := ws.RemoveAll(ctx, ".")
	if err == nil {
		t.Fatal("should refuse to remove root")
	}
	_ = ctx
}

func TestLocalWorkspace_ReadNotFound(t *testing.T) {
	ws, ctx := newLocalWS(t)

	_, err := ws.Read(ctx, "nonexistent.txt")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestLocalWorkspace_DeleteNotFound(t *testing.T) {
	ws, ctx := newLocalWS(t)

	err := ws.Delete(ctx, "nonexistent.txt")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestLocalWorkspace_PathTraversal(t *testing.T) {
	ws, ctx := newLocalWS(t)

	_, err := ws.Read(ctx, "../etc/passwd")
	if err == nil {
		t.Fatal("expected path traversal error")
	}

	err = ws.Write(ctx, "/etc/passwd", []byte("x"))
	if err == nil {
		t.Fatal("expected absolute path rejection")
	}
}

func TestLocalWorkspace_ExistsNotFound(t *testing.T) {
	ws, ctx := newLocalWS(t)

	exists, err := ws.Exists(ctx, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("should not exist")
	}
}

func TestLocalWorkspace_SymlinkEscape(t *testing.T) {
	skipWindows(t)
	ws, ctx := newLocalWS(t)

	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("leaked"), 0o644)
	os.Symlink(outside, filepath.Join(ws.Root(), "escape"))

	if _, err := ws.Read(ctx, "escape/secret.txt"); err == nil {
		t.Fatal("expected symlink escape to be blocked on read")
	}
	if err := ws.Write(ctx, "escape/new.txt", []byte("pwned")); err == nil {
		t.Fatal("expected symlink escape to be blocked on write")
	}
	if exists, err := ws.Exists(ctx, "escape/secret.txt"); err == nil && exists {
		t.Fatal("expected symlink escape to be blocked on exists")
	}
	if _, err := ws.Stat(ctx, "escape/secret.txt"); err == nil {
		t.Fatal("expected symlink escape to be blocked on stat")
	}
	if _, err := ws.List(ctx, "escape"); err == nil {
		t.Fatal("expected symlink escape to be blocked on list")
	}
	if err := ws.Append(ctx, "escape/log.txt", []byte("x")); err == nil {
		t.Fatal("expected symlink escape to be blocked on append")
	}
	if err := ws.Delete(ctx, "escape/secret.txt"); err == nil {
		t.Fatal("expected symlink escape to be blocked on delete")
	}
	if err := ws.RemoveAll(ctx, "escape"); err == nil {
		t.Fatal("expected symlink escape to be blocked on removeall")
	}
}

func TestLocalWorkspace_SymlinkFileEscape(t *testing.T) {
	skipWindows(t)
	ws, ctx := newLocalWS(t)

	outside := t.TempDir()
	secretFile := filepath.Join(outside, "passwd")
	os.WriteFile(secretFile, []byte("root:x:0:0"), 0o644)
	os.Symlink(secretFile, filepath.Join(ws.Root(), "passwd"))

	if _, err := ws.Read(ctx, "passwd"); err == nil {
		t.Fatal("expected symlink file escape to be blocked")
	}
}

func TestLocalWorkspace_InternalSymlinkAllowed(t *testing.T) {
	skipWindows(t)
	ws, ctx := newLocalWS(t)

	ws.Write(ctx, "real/data.txt", []byte("internal"))
	os.Symlink(filepath.Join(ws.Root(), "real"), filepath.Join(ws.Root(), "link"))

	data, err := ws.Read(ctx, "link/data.txt")
	if err != nil {
		t.Fatalf("internal symlink should be allowed: %v", err)
	}
	if string(data) != "internal" {
		t.Fatalf("got %q, want 'internal'", data)
	}
}

func TestLocalWorkspace_ResolveEmptyAndDot(t *testing.T) {
	ws, ctx := newLocalWS(t)

	ws.Write(ctx, "f.txt", []byte("x"))

	entries, err := ws.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("listing empty path should return root contents")
	}

	entries2, err := ws.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) != len(entries) {
		t.Fatal("empty and '.' should be equivalent")
	}
}

func TestValidateGitURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
	}{
		{"https://github.com/user/repo.git", false},
		{"git@github.com:user/repo.git", false},
		{"", true},
		{"  ", true},
		{"-upload-pack=evil", true},
		{"--upload-pack=evil", true},
	}
	for _, tt := range tests {
		err := validateGitURL(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateGitURL(%q) error = %v, wantErr = %v", tt.url, err, tt.wantErr)
		}
	}
}

// --- helpers ---

func TestLocalWorkspace_Rename(t *testing.T) {
	ws, ctx := newLocalWS(t)
	if err := ws.Write(ctx, "a/old.txt", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := ws.Rename(ctx, "a/old.txt", "b/new.txt"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := ws.Exists(ctx, "a/old.txt"); exists {
		t.Fatal("src must not exist after rename")
	}
	data, err := ws.Read(ctx, "b/new.txt")
	if err != nil || string(data) != "payload" {
		t.Fatalf("read dst: data=%q err=%v", data, err)
	}
	// rename overwrites existing dst.
	if err := ws.Write(ctx, "src.txt", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if err := ws.Rename(ctx, "src.txt", "b/new.txt"); err != nil {
		t.Fatal(err)
	}
	data2, _ := ws.Read(ctx, "b/new.txt")
	if string(data2) != "v2" {
		t.Fatalf("rename should overwrite; got %q", data2)
	}
}

func TestLocalWorkspace_Rename_SrcNotFound(t *testing.T) {
	ws, ctx := newLocalWS(t)
	if err := ws.Rename(ctx, "missing.txt", "dst.txt"); err == nil {
		t.Fatal("expected error when src missing")
	}
}

func newLocalWS(t *testing.T) (*LocalWorkspace, context.Context) {
	t.Helper()
	ws, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws, context.Background()
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
}
