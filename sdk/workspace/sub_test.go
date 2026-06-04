package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSubWorkspace_PrefixesOperations(t *testing.T) {
	ctx := context.Background()
	base := NewMemWorkspace()
	sub := Sub(base, "runtime-a")
	if err := sub.Write(ctx, "docs/a.txt", []byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := base.Read(ctx, "runtime-a/docs/a.txt")
	if err != nil {
		t.Fatalf("base Read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
	if err := sub.Append(ctx, "docs/a.txt", []byte(" world")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := sub.Rename(ctx, "docs/a.txt", "docs/b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if ok, err := base.Exists(ctx, "runtime-a/docs/b.txt"); err != nil || !ok {
		t.Fatalf("base Exists = %v, %v; want true, nil", ok, err)
	}
	entries, err := sub.List(ctx, "docs")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == "b.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("entries = %+v, want b.txt", entries)
	}
	if _, err := sub.Stat(ctx, "docs/b.txt"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := sub.Delete(ctx, "docs/b.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, err := base.Exists(ctx, "runtime-a/docs/b.txt"); err != nil || ok {
		t.Fatalf("base Exists after delete = %v, %v; want false, nil", ok, err)
	}
}

func TestSubWorkspace_RejectsTraversal(t *testing.T) {
	ctx := context.Background()
	base := NewMemWorkspace()
	bad := Sub(base, "../runtime-a")
	if err := bad.Write(ctx, "x.txt", []byte("x")); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("bad sub Write err = %v, want ErrPathTraversal", err)
	}
	sub := Sub(base, "runtime-a")
	if err := sub.Write(ctx, "../escape.txt", []byte("x")); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("Write traversal err = %v, want ErrPathTraversal", err)
	}
	if ok, err := base.Exists(ctx, "escape.txt"); err != nil || ok {
		t.Fatalf("escape Exists = %v, %v; want false, nil", ok, err)
	}
}

func TestSubWorkspace_RemoveAllRootRefused(t *testing.T) {
	ctx := context.Background()
	base := NewMemWorkspace()
	base.MustWrite("runtime-a/child/file.txt", []byte("x"))
	sub := Sub(base, "runtime-a")
	if err := sub.RemoveAll(ctx, "."); err == nil {
		t.Fatal("RemoveAll root should fail")
	}
	if err := sub.RemoveAll(ctx, "child"); err != nil {
		t.Fatalf("RemoveAll child: %v", err)
	}
	if ok, err := base.Exists(ctx, "runtime-a/child/file.txt"); err != nil || ok {
		t.Fatalf("child Exists = %v, %v; want false, nil", ok, err)
	}
}

func TestSubWorkspace_Capabilities(t *testing.T) {
	base := NewMemWorkspace()
	sub := Sub(base, "runtime-a")
	got := CapabilitiesOf(sub)
	if !got.AtomicRename || !got.ReadAfterWrite || got.DurableOnWrite || got.Distributed {
		t.Fatalf("CapabilitiesOf(sub) = %+v", got)
	}
}

func TestSubWorkspace_MergesNestedSubPrefixes(t *testing.T) {
	ctx := context.Background()
	base := NewMemWorkspace()
	sub := Sub(Sub(base, "runtime-a"), "memory")
	if err := sub.Write(ctx, "state.json", []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ok, err := base.Exists(ctx, "runtime-a/memory/state.json"); err != nil || !ok {
		t.Fatalf("merged path Exists = %v, %v; want true, nil", ok, err)
	}
}

func TestSubWorkspace_LocalRoot(t *testing.T) {
	base, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	sub := Sub(base, filepath.Join("runtime-a", "memory"))
	rooted, ok := sub.(*LocalWorkspace)
	if !ok {
		t.Fatalf("sub = %T, want *LocalWorkspace", sub)
	}
	want := filepath.Join(base.Root(), "runtime-a", "memory")
	if rooted.Root() != want {
		t.Fatalf("Root() = %q, want %q", rooted.Root(), want)
	}
	if info, err := os.Stat(rooted.Root()); err != nil || !info.IsDir() {
		t.Fatalf("sub root stat = %v, %v; want dir", info, err)
	}
}

func TestSubWorkspace_LocalRootRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup is platform-sensitive on windows")
	}
	base, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base.Root(), "escape")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	sub := Sub(base, "escape")
	if err := sub.Write(context.Background(), "file.txt", []byte("x")); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("sub Write through symlink err = %v, want ErrPathTraversal", err)
	}
}

func TestSubWorkspace_LocalSubPreservesGitWorkspace(t *testing.T) {
	base, err := NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	sub := Sub(base, "runtime-a")
	_, ok := sub.(GitWorkspace)
	if !ok {
		t.Fatalf("local sub does not implement GitWorkspace: %T", sub)
	}
}

func TestSubWorkspace_GenericGitWorkspaceIsPlainWorkspace(t *testing.T) {
	ctx := context.Background()
	git := &recordingGitWorkspace{Workspace: NewMemWorkspace()}
	sub := Sub(git, "runtime-a")
	if _, ok := sub.(GitWorkspace); ok {
		t.Fatalf("generic sub unexpectedly implements GitWorkspace: %T", sub)
	}
	if err := sub.Write(ctx, "file.txt", []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ok, err := git.Exists(ctx, "runtime-a/file.txt"); err != nil || !ok {
		t.Fatalf("prefixed file Exists = %v, %v; want true, nil", ok, err)
	}
}

type recordingGitWorkspace struct {
	Workspace
	calls []string
}

func (r *recordingGitWorkspace) GitClone(_ context.Context, _, dest string) error {
	r.calls = append(r.calls, "clone:"+dest)
	return nil
}

func (r *recordingGitWorkspace) GitPull(_ context.Context, dir string) error {
	r.calls = append(r.calls, "pull:"+dir)
	return nil
}

func (r *recordingGitWorkspace) GitHead(_ context.Context, dir string) (string, error) {
	r.calls = append(r.calls, "head:"+dir)
	return "abc123", nil
}
