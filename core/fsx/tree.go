package fsx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/paths"
	"github.com/GizClaw/flowcraft/core/permission"
)

// Option configures a Tree.
type Option func(*config)

type config struct {
	writes []string
}

// WithWritable declares the absolute paths writes are allowed to touch. Paths
// are subtree-based: /w covers /w and everything beneath it.
func WithWritable(paths ...string) Option {
	return func(c *config) { c.writes = append(c.writes, paths...) }
}

// entry is one writable root: the canonical path and the handle that enforces
// it. The handle is the boundary; the path only selects which handle to try.
type entry struct {
	path string
	root *os.Root
}

// Tree is the file boundary: unrestricted reads, writes confined to the
// declared roots by an OS handle per root.
//
// A handle cannot express "every volume" at once, which is why a Tree takes an
// explicit list rather than a mode: a host that needs broader writes opens
// higher directories.
type Tree struct {
	entries []entry
	closed  bool
}

// Open builds a Tree from options.
//
// Every writable path must exist and be a directory: an entry that cannot be
// opened cannot be enforced, and a writable set nothing enforces is a comment.
// Paths are resolved through symlinks at Open, so the handle and the selector
// compare the same spelling.
func Open(opts ...Option) (*Tree, error) {
	c := &config{}
	for _, opt := range opts {
		opt(c)
	}
	writes := permission.Writes{Paths: c.writes}
	if err := writes.Validate(); err != nil {
		return nil, err
	}

	tree := &Tree{entries: make([]entry, 0, len(c.writes))}
	for _, path := range c.writes {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			tree.Close()
			return nil, errdefs.Validationf(
				"fsx: writable path %q must exist and be a directory: %v", path, err)
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			tree.Close()
			return nil, errdefs.Validationf(
				"fsx: writable path %q is not a directory", path)
		}
		handle, err := os.OpenRoot(real)
		if err != nil {
			tree.Close()
			return nil, fmt.Errorf("fsx: open writable root %q: %w", real, err)
		}
		tree.entries = append(tree.entries, entry{path: real, root: handle})
	}
	return tree, nil
}

// Close releases the handles. A Tree with open handles pins its directories, so
// a host that opens Trees per session must close them.
func (t *Tree) Close() error {
	t.closed = true
	var first error
	for i := range t.entries {
		if t.entries[i].root == nil {
			continue
		}
		if err := t.entries[i].root.Close(); err != nil && first == nil {
			first = err
		}
		t.entries[i].root = nil
	}
	return first
}

// Writable returns the canonical roots this tree may write, in declaration order.
func (t *Tree) Writable() []string {
	out := make([]string, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e.path)
	}
	return out
}

// select maps an absolute target onto the entry whose handle can reach it.
//
// The comparison is a selector, not a decision: if it picks wrongly, the handle
// refuses the relative path it is given. A miss therefore costs a denial, never
// a wider write.
func (t *Tree) select_(abs string) (*entry, string, error) {
	if t.closed {
		return nil, "", fmt.Errorf("fsx: tree is closed")
	}
	// The entries are canonical; the target may still be spelled through a
	// symlink (macOS /var is /private/var), so resolve its existing prefix
	// before comparing. This narrows which handle is tried; it does not decide
	// anything, because the handle resolves the relative path itself.
	resolved := paths.ResolveExisting(abs)
	for i := range t.entries {
		if rel, ok := paths.RelFold(t.entries[i].path, resolved); ok {
			return &t.entries[i], rel, nil
		}
	}
	return nil, "", fmt.Errorf("%w: %s is outside every writable root", ErrDenied, abs)
}

func absolute(op, path string) error {
	if path == "" {
		return errdefs.Validationf("fsx: %s: empty path", op)
	}
	if !filepath.IsAbs(path) {
		return errdefs.Validationf("fsx: %s: path %q must be absolute", op, path)
	}
	return nil
}

func (t *Tree) checkRead(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return errdefs.FromContext(err)
	}
	return absolute("read", path)
}

// denied reports whether the kernel refused the operation rather than the
// operation failing.
func denied(err error) error {
	if err == nil {
		return nil
	}
	if isDenied(err) {
		return fmt.Errorf("%w: %v", ErrDenied, err)
	}
	return err
}
