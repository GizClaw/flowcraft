package fsx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/paths"
)

// Option configures the write side of a Tree. The read side is a positional
// argument to [Open] because it is the primary axis: every session has a read
// answer, and only some narrow it.
type Option func(*config)

type config struct {
	write    []string
	writeAll bool
}

// WithWrite confines writes to the named subtrees. Paths are subtree-based:
// /w covers /w and everything beneath it.
//
// The named directories must exist when the Tree is opened: a handle that
// cannot be opened enforces nothing.
func WithWrite(roots ...string) Option {
	return func(c *config) { c.write = append(c.write, roots...) }
}

// WithAllWrites removes the write restriction. It is mutually exclusive with
// [WithWrite]; passing both is a configuration error rather than a precedence
// puzzle.
func WithAllWrites() Option {
	return func(c *config) { c.writeAll = true }
}

// entry is one root: the canonical path and the handle that enforces it. The
// path selects which handle to try; the handle decides.
type entry struct {
	path string
	root *os.Root
}

// Tree is the file boundary of one session.
//
// A Tree is built by the host before the session runs and must not be
// reconfigured afterwards. The zero value is not usable: build one with [Open].
type Tree struct {
	reads    []entry
	writes   []entry
	writeAll bool
	closed   bool
}

// Open builds a Tree.
//
// read is the set of readable subtrees; an empty set means reads are not
// restricted. Writes are refused unless the caller adds [WithWrite] or
// [WithAllWrites].
//
// Every named root must be an absolute, existing directory: an entry that
// cannot be opened cannot be enforced, and a writable set nothing enforces is a
// comment. Paths are resolved through symlinks once, here, so the handle and the
// selector compare the same spelling for the session's lifetime.
func Open(read []string, opts ...Option) (*Tree, error) {
	c := &config{}
	for _, opt := range opts {
		opt(c)
	}
	if c.writeAll && len(c.write) > 0 {
		return nil, errdefs.Validationf("fsx: WithAllWrites and WithWrite are mutually exclusive")
	}

	tree := &Tree{writeAll: c.writeAll}
	reads, err := openEntries(read)
	if err != nil {
		return nil, err
	}
	tree.reads = reads

	if !c.writeAll {
		writes, err := openEntries(c.write)
		if err != nil {
			_ = tree.Close()
			return nil, err
		}
		tree.writes = writes
	}
	return tree, nil
}

// openEntries canonicalizes a root set and opens one handle per entry, releasing
// everything it opened if one fails.
func openEntries(raw []string) ([]entry, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	roots, err := canonicalRoots(raw)
	if err != nil {
		return nil, err
	}
	entries := make([]entry, 0, len(roots))
	for _, path := range roots {
		handle, err := os.OpenRoot(path)
		if err != nil {
			_ = closeEntries(entries)
			return nil, fmt.Errorf("fsx: open root %q: %w", path, err)
		}
		entries = append(entries, entry{path: path, root: handle})
	}
	return entries, nil
}

// canonicalRoots validates a root set, resolves it, and drops entries nested
// inside another. Two roots where one covers the other would make selection
// depend on order for no benefit, so the narrower one is simply redundant.
func canonicalRoots(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	canonical := make([]string, 0, len(raw))
	for _, root := range raw {
		switch {
		case strings.TrimSpace(root) == "":
			return nil, errdefs.Validationf("fsx: root must not be empty")
		case !filepath.IsAbs(root):
			return nil, errdefs.Validationf("fsx: root %q must be absolute", root)
		case strings.ContainsAny(root, "*?["):
			return nil, errdefs.Validationf("fsx: root %q must not be a glob", root)
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return nil, errdefs.Validationf("fsx: root %q must exist and be a directory: %w", root, err)
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			return nil, errdefs.Validationf("fsx: root %q is not a directory", root)
		}
		if _, dup := seen[real]; dup {
			continue
		}
		seen[real] = struct{}{}
		canonical = append(canonical, real)
	}

	sort.Strings(canonical)
	kept := canonical[:0]
	for _, root := range canonical {
		covered := false
		for _, wider := range kept {
			if paths.WithinFold(wider, root) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, root)
		}
	}
	return kept, nil
}

// Close releases every handle. A Tree with open handles pins its directories, so
// a host that opens Trees per session must close them.
func (t *Tree) Close() error {
	t.closed = true
	return errors.Join(closeEntries(t.reads), closeEntries(t.writes))
}

func closeEntries(entries []entry) error {
	var errs []error
	for i := range entries {
		if entries[i].root == nil {
			continue
		}
		if err := entries[i].root.Close(); err != nil {
			errs = append(errs, err)
		}
		entries[i].root = nil
	}
	return errors.Join(errs...)
}

// Readable returns the canonical readable roots in declaration order, or nil
// when reads are not restricted.
func (t *Tree) Readable() []string { return rootPaths(t.reads) }

// Writable returns the canonical writable roots in declaration order, or nil
// when writes are not restricted.
func (t *Tree) Writable() []string { return rootPaths(t.writes) }

// WriteAll reports whether the tree places no restriction on writes.
func (t *Tree) WriteAll() bool { return t.writeAll }

func rootPaths(entries []entry) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.path)
	}
	return out
}

// readable resolves path against the read side.
func (t *Tree) readable(ctx context.Context, op, path string) (acc, error) {
	if err := t.usable(ctx, op, path); err != nil {
		return acc{}, err
	}
	if len(t.reads) == 0 {
		return acc{name: filepath.Clean(path)}, nil
	}
	return resolve(t.reads, op, path)
}

// writable resolves path against the write side.
func (t *Tree) writable(ctx context.Context, op, path string) (acc, error) {
	if err := t.usable(ctx, op, path); err != nil {
		return acc{}, err
	}
	switch {
	case t.writeAll:
		return acc{name: filepath.Clean(path)}, nil
	case len(t.writes) == 0:
		return acc{}, fmt.Errorf("%w: %s: this session may not write", ErrDenied, path)
	}
	return resolve(t.writes, op, path)
}

// usable applies the checks both sides share: the tree is open, the context is
// live, and the path is an absolute one this boundary can reason about.
func (t *Tree) usable(ctx context.Context, op, path string) error {
	if t.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return errdefs.FromContext(err)
	}
	if path == "" {
		return errdefs.Validationf("fsx: %s: empty path", op)
	}
	if !filepath.IsAbs(path) {
		return errdefs.Validationf("fsx: %s: path %q must be absolute", op, path)
	}
	return nil
}

// resolve maps an absolute target onto the root that contains it.
//
// The comparison is a selector, not a decision: if it picks wrongly, the handle
// refuses the relative path it is given, so a miss costs a denial rather than a
// wider access. The target may still be spelled through a symlink (macOS /var is
// /private/var), so its existing prefix is resolved before comparing.
func resolve(entries []entry, op, path string) (acc, error) {
	resolved := paths.ResolveExisting(path)
	for i := range entries {
		if rel, ok := paths.RelFold(entries[i].path, resolved); ok {
			return acc{root: entries[i].root, base: entries[i].path, name: rel}, nil
		}
	}
	return acc{}, fmt.Errorf("%w: %s: %s is outside every root of this boundary", ErrDenied, path, op)
}
