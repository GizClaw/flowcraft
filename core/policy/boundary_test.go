package policy_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/policy"
)

// boundary is the adapter the host will use: policy decides what "empty" and
// "readonly" mean, fsx knows how to enforce a root set. Neither package imports
// the other, so the translation lives here — three lines, one meaning.
func boundary(t *testing.T, files policy.Files) *fsx.Tree {
	t.Helper()
	normalized, err := files.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	read, write, denyWrite := normalized.Unrestricted()

	var opts []fsx.Option
	switch {
	case denyWrite:
		// readonly: no write grant at all.
	case len(write) == 0:
		opts = append(opts, fsx.WithAllWrites())
	default:
		opts = append(opts, fsx.WithWrite(write...))
	}

	tree, err := fsx.Open(read, opts...)
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree
}

// TestBoundaryIsThePolicyIsTheBatchOneAcceptance is the first end-to-end thing
// the platform can do: a policy value turns into an enforced file boundary, a
// read inside it works, and a write outside it is refused.
func TestBoundaryIsThePolicy(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	work := filepath.Join(root, "work")
	out := filepath.Join(work, "out")
	for _, dir := range []string{work, out} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	inside := filepath.Join(work, "readme.txt")
	if err := os.WriteFile(inside, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tree := boundary(t, policy.Files{Read: []string{work}, Write: []string{out}})
	ctx := t.Context()

	data, err := tree.Read(ctx, inside)
	if err != nil {
		t.Fatalf("read inside the boundary: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("read = %q", data)
	}

	if err := tree.Write(ctx, filepath.Join(out, "ok.txt"), []byte("fine")); err != nil {
		t.Fatalf("write inside the write root: %v", err)
	}
	if err := tree.Write(ctx, inside, []byte("nope")); !errors.Is(err, fsx.ErrDenied) {
		t.Fatalf("write outside the write root = %v, want ErrDenied", err)
	}
}

// TestReadOnlyPolicyGrantsNoFilesAtAll pins the mapping of the one field that
// cannot be written as a root list.
func TestReadOnlyPolicyGrantsNoFilesAtAll(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	// readonly with no read roots: reads stay unrestricted, writes are refused.
	tree := boundary(t, policy.Files{ReadOnly: true})
	if err := tree.Write(t.Context(), filepath.Join(root, "x.txt"), []byte("x")); !errors.Is(err, fsx.ErrDenied) {
		t.Fatalf("write under readonly = %v, want ErrDenied", err)
	}
}
