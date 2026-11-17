// Package engine is the policy layer between the sandbox contract and a backend:
// the one-shot Exec, the decorators, and the shared validation.
//
// The path helpers in this file (Resolve, ResolveMany) decide where a backend
// setting path lands relative to a workspace root. They constrain relative paths
// only, and they run at configuration time: the boundary a command actually runs
// under is the backend's kernel policy, not a string check. Resolve documents
// both limits.
package engine

import (
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/paths"
)

// ResolveMany resolves every path in paths. A nil slice returns nil.
func ResolveMany(root string, paths []string) ([]string, error) {
	if paths == nil {
		return nil, nil
	}
	resolved := make([]string, len(paths))
	for i, p := range paths {
		if p == "" {
			return nil, errdefs.Validationf("path[%d] is empty", i)
		}
		value, err := Resolve(root, p)
		if err != nil {
			return nil, errdefs.Validationf("path[%d] %q: %w", i, p, err)
		}
		resolved[i] = value
	}
	return resolved, nil
}

// Resolve makes a relative setting path relative to root, returns it in its
// resolved form, and rejects relative traversal outside root.
//
// Two limits are deliberate, and neither is a bug:
//
//   - An absolute path is cleaned and returned unchanged. It is not checked
//     against root: a deployment states its writable paths absolutely, and
//     containment here is a relative-path check and nothing more.
//   - Symlinks are evaluated when Resolve runs, so a link swapped afterwards is
//     a TOCTOU window this helper cannot close. The check describes the tree as
//     it was, not as the command will find it.
//
// So this is configuration hygiene, not confinement: a relative setting that
// plainly escapes is a validation error at load instead of a command that fails
// later. The boundary a command runs under is the kernel policy its backend
// installs (the Seatbelt profile, the bwrap namespaces and binds).
func Resolve(root, p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	value := filepath.Clean(filepath.Join(root, p))
	cleanRoot := filepath.Clean(root)
	if !paths.Within(cleanRoot, value) {
		return "", errdefs.Validationf(
			"relative path %q escapes workspace root", p)
	}
	realRoot, err := EvalExistingPrefix(cleanRoot)
	if err != nil {
		return "", errdefs.Validationf(
			"resolve workspace root %q: %w", cleanRoot, err)
	}
	realValue, err := EvalExistingPrefix(value)
	if err != nil {
		return "", errdefs.Validationf(
			"resolve relative path %q: %w", p, err)
	}
	if !paths.Within(realRoot, realValue) {
		return "", errdefs.Validationf(
			"relative path %q escapes workspace root through a symlink", p)
	}
	return realValue, nil
}

func EvalExistingPrefix(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p, nil
	}
	realParent, err := EvalExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(p)), nil
}
