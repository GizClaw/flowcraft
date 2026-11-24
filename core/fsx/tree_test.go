package fsx_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
)

// fixture is one directory tree the tests can point boundaries at:
//
//	<tmp>/work/inside.txt
//	<tmp>/work/sub/nested.txt
//	<tmp>/other/outside.txt
func fixture(t *testing.T) (work, other string) {
	t.Helper()
	tmp := t.TempDir()
	// Canonicalize once: the tree reports roots and walk results in their
	// resolved spelling (on macOS /var is /private/var), so the expectations
	// must start resolved too.
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	work = filepath.Join(tmp, "work")
	other = filepath.Join(tmp, "other")
	for _, dir := range []string{filepath.Join(work, "sub"), other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(work, "inside.txt"):        "inside",
		filepath.Join(work, "sub", "nested.txt"): "nested",
		filepath.Join(other, "outside.txt"):      "outside",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return work, other
}

func mustOpen(t *testing.T, read []string, opts ...fsx.Option) *fsx.Tree {
	t.Helper()
	tree, err := fsx.Open(read, opts...)
	if err != nil {
		t.Fatalf("Open(%v): %v", read, err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree
}

func TestReadsAreConfinedToTheReadRoots(t *testing.T) {
	work, other := fixture(t)
	tree := mustOpen(t, []string{work})
	ctx := context.Background()

	data, err := tree.Read(ctx, filepath.Join(work, "inside.txt"))
	if err != nil {
		t.Fatalf("read inside: %v", err)
	}
	if string(data) != "inside" {
		t.Fatalf("read inside = %q", data)
	}

	if _, err := tree.Read(ctx, filepath.Join(other, "outside.txt")); !errors.Is(err, fsx.ErrDenied) {
		t.Fatalf("read outside the roots = %v, want ErrDenied", err)
	}
}

// TestNoReadRootsMeansUnrestricted pins the asymmetry the package documents:
// omitting read roots does not close the file system, it removes the read
// boundary. Writes still need an explicit grant.
func TestNoReadRootsMeansUnrestricted(t *testing.T) {
	work, other := fixture(t)
	tree := mustOpen(t, nil)
	ctx := context.Background()

	for _, path := range []string{filepath.Join(work, "inside.txt"), filepath.Join(other, "outside.txt")} {
		if _, err := tree.Read(ctx, path); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
	}
	if got := tree.Readable(); got != nil {
		t.Fatalf("Readable() = %v, want nil (unrestricted)", got)
	}
}

func TestWritesAreRefusedWithoutAGrant(t *testing.T) {
	work, _ := fixture(t)
	tree := mustOpen(t, []string{work})
	ctx := context.Background()

	err := tree.Write(ctx, filepath.Join(work, "created.txt"), []byte("x"))
	if !errors.Is(err, fsx.ErrDenied) {
		t.Fatalf("write without a grant = %v, want ErrDenied", err)
	}
}

func TestWritesAreConfinedToTheWriteRoots(t *testing.T) {
	work, other := fixture(t)
	sub := filepath.Join(work, "sub")
	tree := mustOpen(t, []string{work}, fsx.WithWrite(sub))
	ctx := context.Background()

	if err := tree.Write(ctx, filepath.Join(sub, "new.txt"), []byte("ok")); err != nil {
		t.Fatalf("write inside the write root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "new.txt")); err != nil {
		t.Fatalf("written file is missing: %v", err)
	}

	for _, path := range []string{
		filepath.Join(work, "sibling.txt"),    // inside the read root, outside the write root
		filepath.Join(other, "elsewhere.txt"), // outside both
	} {
		if err := tree.Write(ctx, path, []byte("x")); !errors.Is(err, fsx.ErrDenied) {
			t.Fatalf("write %s = %v, want ErrDenied", path, err)
		}
	}
}

func TestWriteCreatesMissingParents(t *testing.T) {
	work, _ := fixture(t)
	tree := mustOpen(t, []string{work}, fsx.WithWrite(work))

	path := filepath.Join(work, "a", "b", "c.txt")
	if err := tree.Write(context.Background(), path, []byte("deep")); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "deep" {
		t.Fatalf("read back = %q, %v", data, err)
	}
}

func TestWithAllWritesRemovesTheWriteBoundary(t *testing.T) {
	work, other := fixture(t)
	tree := mustOpen(t, []string{work}, fsx.WithAllWrites())
	ctx := context.Background()

	if err := tree.Write(ctx, filepath.Join(other, "anywhere.txt"), []byte("x")); err != nil {
		t.Fatalf("write with all writes: %v", err)
	}
	if !tree.WriteAll() {
		t.Fatal("WriteAll() = false")
	}
	if got := tree.Writable(); got != nil {
		t.Fatalf("Writable() = %v, want nil (unrestricted)", got)
	}
}

func TestAllWritesAndWriteAreMutuallyExclusive(t *testing.T) {
	work, _ := fixture(t)
	if _, err := fsx.Open(nil, fsx.WithAllWrites(), fsx.WithWrite(work)); err == nil {
		t.Fatal("Open with both write options succeeded, want a validation error")
	}
}

func TestRelativePathsAreRejected(t *testing.T) {
	work, _ := fixture(t)
	tree := mustOpen(t, []string{work}, fsx.WithWrite(work))
	ctx := context.Background()

	if _, err := tree.Read(ctx, "inside.txt"); err == nil {
		t.Fatal("relative read succeeded, want a validation error")
	}
	if err := tree.Write(ctx, "inside.txt", nil); err == nil {
		t.Fatal("relative write succeeded, want a validation error")
	}
}

func TestRootsMustExistAndBeDirectories(t *testing.T) {
	work, _ := fixture(t)
	file := filepath.Join(work, "inside.txt")

	for name, root := range map[string]string{
		"missing":  filepath.Join(work, "nope"),
		"file":     file,
		"relative": "work",
		"glob":     filepath.Join(work, "*"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fsx.Open([]string{root}); err == nil {
				t.Fatalf("Open(%q) succeeded, want a validation error", root)
			}
		})
	}
}

// TestNestedRootsCollapse pins normalization: a root covered by another is
// redundant, and keeping both would make selection depend on declaration order.
func TestNestedRootsCollapse(t *testing.T) {
	work, _ := fixture(t)
	sub := filepath.Join(work, "sub")

	tree := mustOpen(t, []string{work, sub}, fsx.WithWrite(work, sub))
	readable := tree.Readable()
	if len(readable) != 1 {
		t.Fatalf("Readable() = %v, want one canonical root", readable)
	}
	if writable := tree.Writable(); len(writable) != 1 {
		t.Fatalf("Writable() = %v, want one canonical root", writable)
	}
}

// TestSymlinkEscapeIsRefused is the reason the boundary is a handle rather than a
// string check: a link inside the root that points outside must not be followed.
func TestSymlinkEscapeIsRefused(t *testing.T) {
	work, other := fixture(t)
	link := filepath.Join(work, "escape")
	if err := os.Symlink(other, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tree := mustOpen(t, []string{work})

	if _, err := tree.Read(context.Background(), filepath.Join(link, "outside.txt")); err == nil {
		t.Fatal("read through an escaping symlink succeeded")
	}
}

func TestWalkReportsAbsolutePathsAndSkipsTheRoot(t *testing.T) {
	work, _ := fixture(t)
	tree := mustOpen(t, []string{work})

	var walked []string
	err := tree.Walk(context.Background(), work, func(path string, _ os.DirEntry) error {
		walked = append(walked, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	want := map[string]bool{
		filepath.Join(work, "inside.txt"):        true,
		filepath.Join(work, "sub"):               true,
		filepath.Join(work, "sub", "nested.txt"): true,
	}
	for _, path := range walked {
		if path == work {
			t.Fatalf("walk reported the root itself: %v", walked)
		}
		if !filepath.IsAbs(path) {
			t.Fatalf("walk reported a relative path %q in %v", path, walked)
		}
		delete(want, path)
	}
	if len(want) != 0 {
		t.Fatalf("walk missed %v (got %v)", want, walked)
	}
}

func TestRemoveRefusesTheRootItself(t *testing.T) {
	work, _ := fixture(t)
	tree := mustOpen(t, []string{work}, fsx.WithWrite(work))

	if err := tree.Remove(context.Background(), work); err == nil {
		t.Fatal("removing the writable root succeeded")
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("root was removed: %v", err)
	}
}

func TestCopyCrossesTheBoundaryOneSideAtATime(t *testing.T) {
	work, _ := fixture(t)
	tree := mustOpen(t, []string{work}, fsx.WithWrite(work))
	ctx := context.Background()

	src := filepath.Join(work, "inside.txt")
	dst := filepath.Join(work, "sub", "copied.txt")
	if err := tree.Copy(ctx, src, dst); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "inside" {
		t.Fatalf("copied = %q, %v", data, err)
	}
}

func TestCloseReleasesAndRefusesFurtherUse(t *testing.T) {
	work, _ := fixture(t)
	tree, err := fsx.Open([]string{work})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := tree.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := tree.Read(context.Background(), filepath.Join(work, "inside.txt")); !errors.Is(err, fsx.ErrClosed) {
		t.Fatalf("read on a closed tree = %v, want ErrClosed", err)
	}
}
