package permission

import (
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Access is what a file entry grants or forbids.
type Access uint8

const (
	// Read permits reading a path. Read entries are interpreted in-process
	// (fsx) only: no sandbox backend projects them onto the kernel, so a
	// child process can still read a path this entry denies. Use it as a
	// tool-layer guard, never as the boundary that protects a secret; keep
	// those outside the workload root instead.
	Read Access = iota
	// Write permits writing a path. This is the one file permission both
	// interpreters agree on: fsx refuses writes in-process, and a sandbox
	// backend bind-mounts exactly these paths writable.
	Write
	// Deny forbids a path for the direction it is written against, and wins
	// over an allow entry that also matches. Deny shares Read's limitation:
	// only fsx enforces it.
	Deny
)

// Entry grants or forbids one absolute path.
//
// Path is absolute and glob-free on purpose. A sandbox projection is a mount:
// it can bind one path or one subtree, but it cannot express "every directory
// named tmp at any depth" without scanning the filesystem first. Accepting a
// glob here would mean admitting a permission the kernel cannot enforce, which
// is how two drifting boundaries start.
type Entry struct {
	Path   string
	Access Access
}

// Files is the file half of a Policy.
type Files struct {
	Entries []Entry
}

// Allows reports whether path may be accessed in the want direction.
//
// Containment is lexical and subtree-based: a Write entry for /w permits /w
// and everything beneath it. Deny is checked first for both directions, so a
// Deny entry always wins. With no Entries at all, reads are unrestricted and
// writes are refused, which is the read-only default.
func (f Files) Allows(path string, want Access) bool {
	if path == "" {
		return false
	}
	for _, entry := range f.Entries {
		if entry.Access == Deny && contains(entry.Path, path) {
			return false
		}
	}
	if want == Read {
		for _, entry := range f.Entries {
			if entry.Access == Read && contains(entry.Path, path) {
				return true
			}
		}
		// No Read entries means "no read restriction"; Deny above already had
		// its chance, so this reports true rather than silently closing reads.
		return len(f.Entries) == 0
	}
	for _, entry := range f.Entries {
		if entry.Access == Write && contains(entry.Path, path) {
			return true
		}
	}
	return false
}

// Writable returns the absolute paths a sandbox backend must bind writable, in
// declaration order. It is the projection input: every Write entry becomes a
// writable mount, and nothing else does.
func (f Files) Writable() []string {
	var paths []string
	for _, entry := range f.Entries {
		if entry.Access == Write {
			paths = append(paths, entry.Path)
		}
	}
	return paths
}

// HasReadRestrictions reports whether the entry list constrains reads at all.
// Backends that cannot enforce read restrictions use it to refuse a policy
// instead of quietly widening it.
func (f Files) HasReadRestrictions() bool {
	for _, entry := range f.Entries {
		if entry.Access == Read || entry.Access == Deny {
			return true
		}
	}
	return false
}

func contains(root, path string) bool {
	if root == "" {
		return false
	}
	return Within(root, path)
}

// validate rejects entries no interpreter can honour: relative paths, and
// globs, which a mount projection would have to materialise by scanning.
func (f Files) validate() error {
	for i, entry := range f.Entries {
		if !filepath.IsAbs(entry.Path) {
			return errdefs.Validationf("permission: file entry %d path %q must be absolute", i, entry.Path)
		}
		if strings.ContainsAny(entry.Path, "*?[") {
			return errdefs.Validationf("permission: file entry %d path %q must not be a glob", i, entry.Path)
		}
		if entry.Access > Deny {
			return errdefs.Validationf("permission: file entry %d has unknown access %d", i, int(entry.Access))
		}
	}
	return nil
}
