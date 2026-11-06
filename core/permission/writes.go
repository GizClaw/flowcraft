package permission

import (
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Writes is the file permission this framework enforces: an allow-list of
// absolute paths a turn may write.
//
// Only writes are listed because only writes are restricted. Reads are
// deliberately outside this vocabulary: what a read rule protects is a secret,
// and a secret belongs outside the tree the agent works in. A read rule would
// only protect the code paths that consult it, and it would have to hold
// identically for in-process tools and for every command a sandbox runs — two
// projections to keep in sync for a failure that does not persist. See the
// package doc.
//
// Paths are absolute and subtree-based: /w covers /w and everything beneath it.
// They are glob-free because a sandbox projection is a mount, and a mount binds
// a path or a subtree — not "anything named tmp at any depth".
type Writes struct {
	Paths []string
}

// Writable returns the paths in declaration order. It is the projection input:
// every entry becomes a writable mount, and nothing else does.
func (w Writes) Writable() []string {
	if len(w.Paths) == 0 {
		return nil
	}
	out := make([]string, len(w.Paths))
	copy(out, w.Paths)
	return out
}

// IsZero reports whether nothing is writable.
func (w Writes) IsZero() bool { return len(w.Paths) == 0 }

// Validate rejects entries no sandbox projection can honour: relative paths,
// globs, and duplicates.
func (w Writes) Validate() error {
	seen := make(map[string]bool, len(w.Paths))
	for i, entry := range w.Paths {
		if !filepath.IsAbs(entry) {
			return errdefs.Validationf(
				"permission: writable path %d (%q) must be absolute", i, entry)
		}
		if strings.ContainsAny(entry, "*?[") {
			return errdefs.Validationf(
				"permission: writable path %d (%q) must not be a glob; a mount binds paths, not patterns", i, entry)
		}
		clean := filepath.Clean(entry)
		if seen[clean] {
			return errdefs.Validationf(
				"permission: writable path %d (%q) is declared twice", i, entry)
		}
		seen[clean] = true
	}
	return nil
}
