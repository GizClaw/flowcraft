package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAt(t *testing.T, tree interface {
	Write(context.Context, string, []byte) error
}, path, body string) {
	t.Helper()
	if err := tree.Write(context.Background(), path, []byte(body)); err != nil {
		t.Fatalf("Write %s: %v", path, err)
	}
}

func TestEditReplacesAUniqueMatch(t *testing.T) {
	fileTools, tree := group(t)
	path := filepath.Join(tree.Writable()[0], "a.txt")
	writeAt(t, tree, path, "alpha\nbeta\ngamma\n")

	text, err := call(t, find(t, fileTools, EditName), map[string]any{
		"file_path": path, "old_string": "beta", "new_string": "BETA",
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := decode(t, text)["replaced"].(float64); got != 1 {
		t.Errorf("replaced = %v, want 1", got)
	}
	got, err := tree.Read(context.Background(), path)
	if err != nil || string(got) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("content = %q, %v", got, err)
	}
}

// A missing match says so: "changed nothing" and "changed something" must not
// look alike.
func TestEditReportsMissingMatch(t *testing.T) {
	fileTools, tree := group(t)
	path := filepath.Join(tree.Writable()[0], "a.txt")
	writeAt(t, tree, path, "alpha\n")

	_, err := call(t, find(t, fileTools, EditName), map[string]any{
		"file_path": path, "old_string": "absent", "new_string": "x",
	})
	if err == nil {
		t.Fatal("missing match accepted")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file", err)
	}
}

// An ambiguous match is refused rather than replaced arbitrarily.
func TestEditRefusesAmbiguousMatch(t *testing.T) {
	fileTools, tree := group(t)
	path := filepath.Join(tree.Writable()[0], "a.txt")
	writeAt(t, tree, path, "x\nx\n")

	_, err := call(t, find(t, fileTools, EditName), map[string]any{
		"file_path": path, "old_string": "x", "new_string": "y",
	})
	if err == nil {
		t.Fatal("ambiguous match accepted")
	}
	if !strings.Contains(err.Error(), "2 times") {
		t.Errorf("error %q does not report the count", err)
	}
	got, _ := tree.Read(context.Background(), path)
	if string(got) != "x\nx\n" {
		t.Errorf("refused edit changed the file: %q", got)
	}
}

func TestEditReplaceAll(t *testing.T) {
	fileTools, tree := group(t)
	path := filepath.Join(tree.Writable()[0], "a.txt")
	writeAt(t, tree, path, "x\nx\n")

	if _, err := call(t, find(t, fileTools, EditName), map[string]any{
		"file_path": path, "old_string": "x", "new_string": "y", "replace_all": true,
	}); err != nil {
		t.Fatalf("edit replace_all: %v", err)
	}
	got, _ := tree.Read(context.Background(), path)
	if string(got) != "y\ny\n" {
		t.Errorf("content = %q, want both replaced", got)
	}
}

// Reads are unrestricted but writes are not: editing outside the roots fails at
// the write, and the denial names the roots.
func TestEditOutsideWritableRootIsDenied(t *testing.T) {
	fileTools, tree := group(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := call(t, find(t, fileTools, EditName), map[string]any{
		"file_path": outside, "old_string": "hello", "new_string": "bye",
	})
	if err == nil {
		t.Fatal("edit outside the writable roots accepted")
	}
	if !strings.Contains(err.Error(), tree.Writable()[0]) {
		t.Errorf("denial %q does not name the writable root", err)
	}
}

func TestEditRequiresOldString(t *testing.T) {
	fileTools, tree := group(t)
	path := filepath.Join(tree.Writable()[0], "a.txt")
	writeAt(t, tree, path, "alpha\n")
	if _, err := call(t, find(t, fileTools, EditName), map[string]any{
		"file_path": path, "new_string": "x",
	}); err == nil {
		t.Fatal("edit without old_string accepted")
	}
}
