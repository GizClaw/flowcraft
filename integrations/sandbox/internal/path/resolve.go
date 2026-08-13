// Package path resolves backend setting paths relative to a workspace
// root, rejecting traversal outside it (including through symlinks).
package path

import (
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
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
			return nil, errdefs.Validationf("path[%d] %q: %v", i, p, err)
		}
		resolved[i] = value
	}
	return resolved, nil
}

// Resolve makes a relative setting path relative to root and rejects
// relative traversal outside it. Absolute paths are cleaned and
// returned unchanged.
func Resolve(root, p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	value := filepath.Clean(filepath.Join(root, p))
	cleanRoot := filepath.Clean(root)
	relative, err := filepath.Rel(cleanRoot, value)
	if err != nil {
		return "", errdefs.Validationf(
			"resolve relative path %q: %v", p, err)
	}
	if relative == ".." ||
		(len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator)) {
		return "", errdefs.Validationf(
			"relative path %q escapes workspace root", p)
	}
	realRoot, err := evalExistingPrefix(cleanRoot)
	if err != nil {
		return "", errdefs.Validationf(
			"resolve workspace root %q: %v", cleanRoot, err)
	}
	realValue, err := evalExistingPrefix(value)
	if err != nil {
		return "", errdefs.Validationf(
			"resolve relative path %q: %v", p, err)
	}
	relative, err = filepath.Rel(realRoot, realValue)
	if err != nil || relative == ".." ||
		(len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator)) {
		return "", errdefs.Validationf(
			"relative path %q escapes workspace root through a symlink", p)
	}
	return realValue, nil
}

func evalExistingPrefix(p string) (string, error) {
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
	realParent, err := evalExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(p)), nil
}
