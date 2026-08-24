//go:build windows

package windows

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// resolveRoot canonicalises the runner root at construction time so a
// later symlink swap on the root itself cannot be used to escape.
// filepath.EvalSymlinks resolves junction points on Windows; a P2
// hardening step can switch to GetFinalPathNameByHandle for
// short-name / case-normalisation parity.
func resolveRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errdefs.Validationf("windows: resolve path %q: %v", path, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errdefs.Validationf("windows: path %q must exist: %v", path, err)
	}
	return real, nil
}

// resolveWorkDir bounds a WorkDir to the runner root, using the same
// existing-prefix symlink resolution the other backends use.
// Windows note: the comparison is case-insensitive by construction
// because EvalSymlinks returns the on-disk casing; keep using the
// resolved path for the child's cwd, not the caller's spelling.
func resolveWorkDir(root, dir string) (string, error) {
	if dir == "" {
		return root, nil
	}
	abs := dir
	if !filepath.IsAbs(dir) {
		abs = filepath.Join(root, dir)
	}
	abs = filepath.Clean(abs)

	real, err := sandbox.EvalExistingPrefix(abs)
	if err != nil {
		return "", fmt.Errorf("windows: resolve workdir: %w", err)
	}
	if !strings.EqualFold(real, root) &&
		!strings.HasPrefix(strings.ToLower(real), strings.ToLower(root)+strings.ToLower(string(filepath.Separator))) {
		return "", fmt.Errorf("%w: workdir %q escapes root", sandbox.ErrPathTraversal, dir)
	}
	return abs, nil
}
