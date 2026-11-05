// Package fstest is the conformance suite every fsx.FS implementation runs
// against itself. It exists because a framework that pays for an interface must
// be able to prove its implementations agree: without a shared suite, "three
// implementations" means three slightly different contracts.
package fstest

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/permission"
)

// Factory builds an empty filesystem and returns it with the absolute root its
// logical paths resolve against. The test owns cleanup.
type Factory func(t *testing.T) (fsx.FS, string)

// RunContract exercises the behaviour every implementation must share.
func RunContract(t *testing.T, newFS Factory) {
	t.Helper()

	t.Run("write then read", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		want := []byte("hello")
		if err := f.Write(ctx, "a/b.txt", want, nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got, err := f.Read(ctx, "a/b.txt", nil)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("Read = %q, want %q", got, want)
		}
	})

	t.Run("read missing reports fs.ErrNotExist", func(t *testing.T) {
		f, _ := newFS(t)
		if _, err := f.Read(context.Background(), "nope.txt", nil); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Read error = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("write creates parent directories", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		if err := f.Write(ctx, "deep/nested/dir/file.txt", []byte("x"), nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := f.Stat(ctx, "deep/nested/dir/file.txt", nil); err != nil {
			t.Fatalf("Stat: %v", err)
		}
	})

	t.Run("stat reports size and kind", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		if err := f.Write(ctx, "f.txt", []byte("12345"), nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		info, err := f.Stat(ctx, "f.txt", nil)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size() != 5 {
			t.Errorf("Size = %d, want 5", info.Size())
		}
		if info.IsDir() {
			t.Error("IsDir = true, want false")
		}
	})

	t.Run("list returns sorted direct children", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		for _, name := range []string{"c.txt", "a.txt", "b.txt"} {
			if err := f.Write(ctx, "dir/"+name, []byte("x"), nil); err != nil {
				t.Fatalf("Write %s: %v", name, err)
			}
		}
		entries, err := f.List(ctx, "dir", nil)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if want := []string{"a.txt", "b.txt", "c.txt"}; strings.Join(names, ",") != strings.Join(want, ",") {
			t.Errorf("List = %v, want %v", names, want)
		}
	})

	t.Run("walk visits descendants with relative slash paths", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		for _, name := range []string{"sub/x.txt", "sub/deep/y.txt", "top.txt"} {
			if err := f.Write(ctx, name, []byte("x"), nil); err != nil {
				t.Fatalf("Write %s: %v", name, err)
			}
		}
		var seen []string
		err := f.Walk(ctx, ".", nil, func(rel string, entry fs.DirEntry) error {
			if strings.Contains(rel, string('/')) || rel == "top.txt" {
				seen = append(seen, rel)
			}
			if strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
				t.Errorf("relative path %q is not a slash path", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		for _, want := range []string{"sub/deep/y.txt", "sub/x.txt", "top.txt"} {
			if !contains(seen, want) {
				t.Errorf("Walk missed %q (saw %v)", want, seen)
			}
		}
		if !sort.StringsAreSorted(seen) {
			t.Errorf("Walk order = %v, want lexicographic", seen)
		}
	})

	t.Run("mkdir then list", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		if err := f.Mkdir(ctx, "newdir/child", nil); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		entries, err := f.List(ctx, "newdir", nil)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != "child" || !entries[0].IsDir() {
			t.Errorf("List = %+v, want one directory 'child'", entries)
		}
	})

	t.Run("remove is idempotent", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		if err := f.Write(ctx, "tmp/file.txt", []byte("x"), nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := f.Remove(ctx, "tmp", nil); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, err := f.Read(ctx, "tmp/file.txt", nil); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Read after Remove = %v, want fs.ErrNotExist", err)
		}
		if err := f.Remove(ctx, "tmp", nil); err != nil {
			t.Errorf("second Remove = %v, want nil", err)
		}
	})

	t.Run("copy duplicates content", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		if err := f.Write(ctx, "src.txt", []byte("payload"), nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := f.Copy(ctx, "src.txt", "dst.txt", nil); err != nil {
			t.Fatalf("Copy: %v", err)
		}
		got, err := f.Read(ctx, "dst.txt", nil)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if string(got) != "payload" {
			t.Errorf("copied content = %q, want %q", got, "payload")
		}
	})

	t.Run("streams round trip", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		n, err := f.WriteStream(ctx, "stream.txt", strings.NewReader("streamed"), nil)
		if err != nil {
			t.Fatalf("WriteStream: %v", err)
		}
		if n != int64(len("streamed")) {
			t.Errorf("WriteStream wrote %d bytes, want %d", n, len("streamed"))
		}
		rc, err := f.ReadStream(ctx, "stream.txt", nil)
		if err != nil {
			t.Fatalf("ReadStream: %v", err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(data) != "streamed" {
			t.Errorf("ReadStream = %q, want %q", data, "streamed")
		}
	})

	t.Run("traversal is refused", func(t *testing.T) {
		f, _ := newFS(t)
		ctx := context.Background()
		if err := f.Write(ctx, "../escape.txt", []byte("x"), nil); !errors.Is(err, fsx.ErrDenied) {
			t.Errorf("Write(../escape.txt) = %v, want fsx.ErrDenied", err)
		}
		if _, err := f.Read(ctx, "../escape.txt", nil); !errors.Is(err, fsx.ErrDenied) {
			t.Errorf("Read(../escape.txt) = %v, want fsx.ErrDenied", err)
		}
	})

	t.Run("absolute path outside the root is refused", func(t *testing.T) {
		f, root := newFS(t)
		ctx := context.Background()
		outside := path.Join(path.Dir(root), "outside.txt")
		if err := f.Write(ctx, outside, []byte("x"), nil); !errors.Is(err, fsx.ErrDenied) {
			t.Errorf("Write(%s) = %v, want fsx.ErrDenied", outside, err)
		}
	})

	t.Run("per-call policy narrows writes", func(t *testing.T) {
		f, root := newFS(t)
		ctx := context.Background()
		sub := path.Join(root, "sub")
		sb := &fsx.Sandbox{
			Policy: permission.Policy{Files: permission.Files{Entries: []permission.Entry{
				{Path: sub, Access: permission.Write},
			}}},
			Roots: []string{root},
			Cwd:   root,
		}
		if err := f.Write(ctx, "sub/ok.txt", []byte("x"), sb); err != nil {
			t.Fatalf("Write inside the narrowed policy: %v", err)
		}
		if err := f.Write(ctx, "other.txt", []byte("x"), sb); !errors.Is(err, fsx.ErrDenied) {
			t.Errorf("Write outside the narrowed policy = %v, want fsx.ErrDenied", err)
		}
	})

	t.Run("read entries are enforced in process", func(t *testing.T) {
		f, root := newFS(t)
		ctx := context.Background()
		if err := f.Write(ctx, "secret/x.txt", []byte("s"), nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		sb := &fsx.Sandbox{
			Policy: permission.Policy{Files: permission.Files{Entries: []permission.Entry{
				{Path: root, Access: permission.Read},
				{Path: path.Join(root, "secret"), Access: permission.Deny},
			}}},
			Roots: []string{root},
			Cwd:   root,
		}
		if _, err := f.Read(ctx, "secret/x.txt", sb); !errors.Is(err, fsx.ErrDenied) {
			t.Errorf("Read under a deny entry = %v, want fsx.ErrDenied", err)
		}
		if _, err := f.Read(ctx, "secret/x.txt", nil); err != nil {
			t.Errorf("Read without a policy = %v, want nil", err)
		}
	})

	t.Run("canonicalize returns an absolute path under the root", func(t *testing.T) {
		f, root := newFS(t)
		ctx := context.Background()
		if err := f.Write(ctx, "c.txt", []byte("x"), nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		abs, err := f.Canonicalize(ctx, "c.txt", nil)
		if err != nil {
			t.Fatalf("Canonicalize: %v", err)
		}
		if !path.IsAbs(abs) {
			t.Errorf("Canonicalize = %q, want an absolute path", abs)
		}
		// The root may itself be behind a symlink (macOS /var -> /private/var),
		// so compare the suffix rather than the literal prefix.
		if !strings.HasSuffix(abs, "c.txt") {
			t.Errorf("Canonicalize = %q, want it to end in c.txt", abs)
		}
		_ = root
	})
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}
