package fsx_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/fsx"
)

func openTree(t *testing.T, roots ...string) (*fsx.Tree, context.Context) {
	t.Helper()
	tree, err := fsx.Open(fsx.WithWritable(roots...))
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree, context.Background()
}

func TestOpenRejectsUnusableRoots(t *testing.T) {
	if _, err := fsx.Open(fsx.WithWritable("relative/dir")); err == nil {
		t.Error("relative root accepted, want a validation error")
	}
	if _, err := fsx.Open(fsx.WithWritable(filepath.Join(t.TempDir(), "missing"))); err == nil {
		t.Error("missing root accepted, want an error: a handle cannot enforce what it cannot open")
	}
	if _, err := fsx.Open(fsx.WithWritable("/")); err != nil {
		t.Errorf("filesystem root rejected: %v", err)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	tree, ctx := openTree(t, root)

	path := filepath.Join(root, "nested", "a.txt")
	if err := tree.Write(ctx, path, []byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := tree.Read(ctx, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("Read = %q, want %q", got, "hello")
	}
}

func TestWriteOutsideEveryRootIsDenied(t *testing.T) {
	root := t.TempDir()
	tree, ctx := openTree(t, root)

	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	err := tree.Write(ctx, outside, []byte("x"))
	if !errors.Is(err, fsx.ErrDenied) {
		t.Errorf("Write(%s) = %v, want fsx.ErrDenied", outside, err)
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Errorf("denied write created %s: %v", outside, statErr)
	}
}

// A symlink inside the root must not be usable to leave it: the handle
// resolves the link relative to the root and refuses the escape.
func TestSymlinkEscapeIsDenied(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	tree, ctx := openTree(t, root)
	err := tree.Write(ctx, filepath.Join(root, "escape", "secret.txt"), []byte("x"))
	if !errors.Is(err, fsx.ErrDenied) {
		t.Errorf("write through an escaping symlink = %v, want fsx.ErrDenied", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "secret.txt")); !os.IsNotExist(statErr) {
		t.Errorf("denied write created a file outside the root: %v", statErr)
	}
}

func TestReadsAreUnrestricted(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "data.txt")
	if err := os.WriteFile(outsideFile, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tree, ctx := openTree(t, root)
	got, err := tree.Read(ctx, outsideFile)
	if err != nil {
		t.Fatalf("Read outside the writable roots = %v, want nil (reads are unrestricted)", err)
	}
	if string(got) != "payload" {
		t.Errorf("Read = %q, want %q", got, "payload")
	}
}

func TestMkdirRemoveCopy(t *testing.T) {
	root := t.TempDir()
	tree, ctx := openTree(t, root)

	if err := tree.Mkdir(ctx, filepath.Join(root, "a", "b")); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := tree.Write(ctx, filepath.Join(root, "a", "b", "one.txt"), []byte("one")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tree.Copy(ctx, filepath.Join(root, "a", "b", "one.txt"), filepath.Join(root, "a", "two.txt")); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got, err := tree.Read(ctx, filepath.Join(root, "a", "two.txt")); err != nil || string(got) != "one" {
		t.Fatalf("copied content = %q, %v; want %q, nil", got, err, "one")
	}
	if err := tree.Remove(ctx, filepath.Join(root, "a")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := tree.Remove(ctx, filepath.Join(root, "a")); err != nil {
		t.Errorf("second Remove = %v, want nil (idempotent)", err)
	}
	if err := tree.Remove(ctx, root); err == nil {
		t.Error("Remove(root) = nil, want a refusal")
	}
}

func TestWritableIsCanonical(t *testing.T) {
	root := t.TempDir()
	tree, _ := openTree(t, root)

	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	got := tree.Writable()
	if len(got) != 1 || got[0] != real {
		t.Errorf("Writable() = %v, want [%q]", got, real)
	}
}

// WriteStream must copy into the file handle as it reads. A buffering
// implementation would block until the stream closes, so the first chunk would
// never become observable; this pins the property rather than the memory use.
func TestWriteStreamDoesNotBuffer(t *testing.T) {
	root := t.TempDir()
	tree, ctx := openTree(t, root)
	target := filepath.Join(root, "stream.txt")

	pipeReader, pipeWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := tree.WriteStream(ctx, target, pipeReader)
		done <- err
	}()

	if _, err := pipeWriter.Write([]byte("first")); err != nil {
		t.Fatalf("write to pipe: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if data, err := os.ReadFile(target); err == nil && string(data) == "first" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WriteStream did not make the first chunk visible before the stream closed: it buffers")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := pipeWriter.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteStream: %v", err)
	}
}

type failingReader struct {
	sent bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("source failed")
	}
	r.sent = true
	return copy(p, "partial"), nil
}

// A source failure must surface with the bytes already copied, not be swallowed
// by a Close error or reported as success.
func TestWriteStreamReportsSourceErrors(t *testing.T) {
	root := t.TempDir()
	tree, ctx := openTree(t, root)
	target := filepath.Join(root, "partial.txt")

	n, err := tree.WriteStream(ctx, target, &failingReader{})
	if err == nil {
		t.Fatal("WriteStream = nil error, want the source failure")
	}
	if n != int64(len("partial")) {
		t.Errorf("WriteStream reported %d bytes, want %d", n, len("partial"))
	}
	if data, readErr := os.ReadFile(target); readErr != nil || string(data) != "partial" {
		t.Errorf("partial content = %q, %v; want %q, nil", data, readErr, "partial")
	}
}

func TestClosedTreeRefusesWrites(t *testing.T) {
	root := t.TempDir()
	tree, err := fsx.Open(fsx.WithWritable(root))
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	if err := tree.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tree.Write(context.Background(), filepath.Join(root, "x.txt"), []byte("x")); err == nil {
		t.Error("write after Close = nil, want an error")
	}
}
