// Containment is the rule every FS policy rests on: is this path the root, or a
// descendant of it? It lives here because Policy speaks root-relative paths and
// this is the step that turns an untrusted absolute path into one.
//
// Two strengths are offered, and callers must pick deliberately:
//
//   - Within / WithinFold / Rel / ResolveUnder judge the paths as given,
//     lexically. They are the right choice when the path is already resolved, or
//     when the caller owns the resolution step.
//   - RealWithin / RealDir resolve symlinks first. Use them when an untrusted path
//     is compared against a root: without the symlink pass, a link inside the root
//     can point outside it.
//
// This file owns no I/O policy: which roots exist, what is readable, and what
// happens on a violation stay with the caller.

package permission

import (
	"path/filepath"
	"strings"
)

// Within reports whether path is root itself or a descendant of it.
// The comparison is lexical: both arguments must be in the same form
// (absolute, or relative to the same base) for the answer to mean
// anything. An empty root contains nothing.
func Within(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return !escapes(rel)
}

// WithinFold is Within with case-insensitive comparison, for callers
// classifying paths on filesystems where case does not distinguish
// names. It is a separate function because the choice belongs to the
// caller: on a case-sensitive filesystem two paths that differ only in
// case are different paths, and folding them would widen containment.
func WithinFold(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	if strings.EqualFold(path, root) {
		return true
	}
	return Within(strings.ToLower(root), strings.ToLower(path))
}

// Rel returns target expressed relative to root, or ok=false when
// target is outside root. root itself yields ".". The result uses the
// platform separator, exactly like filepath.Rel.
func Rel(root, target string) (string, bool) {
	if root == "" || target == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || escapes(rel) {
		return "", false
	}
	return rel, true
}

// RelRef reports whether ref is a relative reference that cannot walk
// out of the directory it will be joined onto: not absolute, not
// rooted, not "..", and not starting with "../". The rooted case is
// spelled out because on Windows "\x" is not absolute yet still
// resolves against the current drive's root, which is exactly the
// escape this guard exists for.
//
// This is the sibling of Within for names chosen by someone else — a
// zip entry, a manifest-declared binary, a plugin asset path — before
// they are joined onto a root. An empty ref cleans to "." and reports
// true; callers that need a non-empty reference check that themselves.
func RelRef(ref string) bool {
	clean := filepath.Clean(ref)
	return !filepath.IsAbs(clean) &&
		!strings.HasPrefix(clean, string(filepath.Separator)) &&
		clean != ".." &&
		!strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// RealWithin reports whether path is root itself or a descendant of it
// after resolving symlinks on both sides. A side that does not exist
// yet is resolved through its longest existing ancestor, so it still
// compares against the resolved root (on macOS /var is /private/var, and
// comparing a resolved root against an unresolved child would wrongly
// read as "outside"). A caller that must fail closed on a path it
// cannot resolve resolves it itself and uses Within.
func RealWithin(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	return Within(resolvedOrCleaned(root), resolvedOrCleaned(path))
}

// resolvedOrCleaned resolves symlinks on path. When path itself does not
// exist, the longest existing ancestor is resolved and the remainder is
// re-appended; a relative path that cannot be resolved is only cleaned,
// so the result keeps its form.
func resolvedOrCleaned(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return clean
	}
	dir, base := filepath.Split(clean)
	dir = filepath.Clean(dir)
	if base == "" || dir == clean {
		return clean
	}
	return filepath.Join(resolvedOrCleaned(dir), base)
}

// escapes reports whether one filepath.Rel result leaves the base:
// ".." itself, or anything starting with "../".
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
