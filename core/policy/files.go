package policy

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/paths"
)

// Files is the file permission of a session: what it may read and what it may
// write.
//
// Empty is not the same on both sides, and the asymmetry is the point. An empty
// Read set means reads are unrestricted — reading is recoverable, and "let the
// agent look around" is the ordinary deployment. An empty Write set means writes
// are unrestricted too, so the one state that cannot be written as a root list —
// "nothing" — has its own field, [Files.ReadOnly].
//
// The zero value is therefore "unrestricted in both directions". A deployment
// that wants a boundary states one; a deployment that does not gets the same
// behaviour the platform has always had, and the doc says so out loud.
type Files struct {
	// Read lists readable subtrees. Empty means all.
	Read []string `json:"read,omitempty" yaml:"read,omitempty"`
	// Write lists writable subtrees. Empty means all, unless ReadOnly is set.
	Write []string `json:"write,omitempty" yaml:"write,omitempty"`
	// ReadOnly refuses every write. It is mutually exclusive with Write.
	ReadOnly bool `json:"readonly,omitempty" yaml:"readonly,omitempty"`
}

// Normalized returns the canonical form and rejects a set that cannot be
// enforced.
//
// Normalization is lexical: paths are cleaned, sorted, and deduplicated by
// subtree, and a filesystem root ("/" or "C:\") folds the whole list into
// "unrestricted". Symlinks are *not* resolved here — this package does no I/O —
// so two spellings of the same directory can produce two digests. The layer that
// opens handles resolves them once; a deployment that spells a path consistently
// has nothing to worry about.
func (f Files) Normalized() (Files, error) {
	read, err := normalizeRoots("files.read", f.Read)
	if err != nil {
		return Files{}, err
	}
	write, err := normalizeRoots("files.write", f.Write)
	if err != nil {
		return Files{}, err
	}
	out := Files{Read: read, Write: write, ReadOnly: f.ReadOnly}

	switch {
	case out.ReadOnly && len(out.Write) > 0:
		return Files{}, errdefs.Validationf(
			"policy.files: readonly and write are mutually exclusive (write: %v)", out.Write)
	case len(out.Read) > 0 && len(out.Write) == 0 && !out.ReadOnly:
		// A narrowed read with an unrestricted write is the combination an
		// author reaches for by omission and never means: it lets a session
		// that may look at one subtree write anywhere. Refusing it turns a
		// silent over-grant into a load-time error.
		return Files{}, errdefs.Validationf(
			"policy.files: read is narrowed to %v but write is unrestricted; declare write or readonly", out.Read)
	}
	for _, root := range out.Write {
		if !coveredBy(out.Read, root) {
			return Files{}, errdefs.Validationf(
				"policy.files: %q is writable but not readable; write must be a subset of read", root)
		}
	}
	return out, nil
}

// Validate reports whether the set can be enforced.
// RestrictsWrites reports whether the policy states anything about writing: a write set, or a
// read-only posture.
//
// It is the question a backend has to answer before it accepts a confine. An empty Files
// states nothing — that is the difference between "unlimited" and "none", and it is why an
// unrestricted write set needs no enforcement while a read-only one does.
//
// A narrowed Read always implies a write statement, because Normalized refuses the
// combination "read narrowed, write unrestricted": an author never means it, and a session
// that may look at one subtree while writing anywhere is an over-grant.
func (f Files) RestrictsWrites() bool { return f.ReadOnly || len(f.Write) > 0 }

func (f Files) Validate() error {
	_, err := f.Normalized()
	return err
}

// IsZero reports whether the set constrains nothing in either direction.
func (f Files) IsZero() bool {
	return len(f.Read) == 0 && len(f.Write) == 0 && !f.ReadOnly
}

// Unrestricted reports the read and write roots in the shape a file boundary
// takes: the read roots (nil = all), the write roots (nil = all), and whether
// writes are refused outright.
//
// It exists so the boundary can be built without the boundary package importing
// this one: the vocabulary owns the meaning of "empty", the boundary owns the
// handles.
func (f Files) Unrestricted() (read, write []string, denyWrite bool) {
	return slices.Clone(f.Read), slices.Clone(f.Write), f.ReadOnly
}

// normalizeRoots validates one side and returns its canonical form. A filesystem
// root folds the whole side into "unrestricted", because listing "/" and saying
// nothing are the same permission and must produce the same digest.
func normalizeRoots(label string, raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	canonical := make([]string, 0, len(raw))
	for _, root := range raw {
		switch {
		case strings.TrimSpace(root) == "":
			return nil, errdefs.Validationf("policy.%s: empty path", label)
		case strings.TrimSpace(root) != root:
			return nil, errdefs.Validationf("policy.%s: path %q has leading or trailing whitespace", label, root)
		case !filepath.IsAbs(root):
			return nil, errdefs.Validationf("policy.%s: path %q must be absolute", label, root)
		case strings.ContainsAny(root, "*?["):
			return nil, errdefs.Validationf("policy.%s: path %q must not be a glob", label, root)
		}
		clean := filepath.Clean(root)
		if filepath.Dir(clean) == clean {
			// A filesystem root covers everything; keeping it beside narrower
			// roots would make the answer depend on order.
			return nil, nil
		}
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		canonical = append(canonical, clean)
	}
	return pruneNested(canonical), nil
}

// pruneNested sorts a root set and drops every entry covered by another, so a
// permission has one spelling rather than one per way of listing it.
func pruneNested(roots []string) []string {
	sort.Strings(roots)
	kept := roots[:0]
	for _, root := range roots {
		if coveredBy(kept, root) {
			continue
		}
		kept = append(kept, root)
	}
	return kept
}

// coveredBy reports whether root is one of the listed subtrees or sits inside
// one. An empty list covers nothing: callers decide what "empty" means.
func coveredBy(roots []string, root string) bool {
	for _, wider := range roots {
		if paths.Within(wider, root) {
			return true
		}
	}
	return false
}
