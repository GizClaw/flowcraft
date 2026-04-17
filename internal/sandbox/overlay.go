package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// OverlayDir holds the paths for a single overlay mount instance.
// The merged directory is what gets bind-mounted into the container.
type OverlayDir struct {
	Lower  string // original source (read-only)
	Upper  string // per-session writable layer
	Work   string // overlayfs workdir (required by kernel)
	Merged string // union view: lower + upper
}

// OverlayManager prepares and tears down overlay mounts on the host.
// Each session gets independent upper/work/merged directories so writes
// (e.g. npm install producing node_modules/) never touch the original source.
//
// Requires Linux with overlayfs support. The calling process must have
// CAP_SYS_ADMIN (typically root in the main FlowCraft container).
// On unsupported platforms the caller should fall back to a direct bind mount.
type OverlayManager struct {
	baseDir string // e.g. /tmp/flowcraft-overlays
}

// NewOverlayManager creates a manager that stores per-session overlay layers
// under baseDir.
func NewOverlayManager(baseDir string) (*OverlayManager, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("overlay: create base dir: %w", err)
	}
	return &OverlayManager{baseDir: baseDir}, nil
}

// Prepare creates upper/work/merged directories for sessionID + mountTarget
// and performs the overlay mount. Returns the OverlayDir whose Merged field
// should be used as the bind-mount source for the container.
func (m *OverlayManager) Prepare(sessionID, lowerDir, mountTarget string) (*OverlayDir, error) {
	safeName := sanitizeName(mountTarget)
	root := filepath.Join(m.baseDir, sessionID, safeName)

	od := &OverlayDir{
		Lower:  lowerDir,
		Upper:  filepath.Join(root, "upper"),
		Work:   filepath.Join(root, "work"),
		Merged: filepath.Join(root, "merged"),
	}

	for _, d := range []string{od.Upper, od.Work, od.Merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("overlay: mkdir %s: %w", d, err)
		}
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", od.Lower, od.Upper, od.Work)
	cmd := exec.Command("mount", "-t", "overlay", "overlay", "-o", opts, od.Merged)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("overlay: mount: %w: %s", err, string(out))
	}

	return od, nil
}

// Cleanup unmounts and removes all overlay directories for a session.
func (m *OverlayManager) Cleanup(sessionID string) error {
	sessionDir := filepath.Join(m.baseDir, sessionID)
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("overlay: read session dir: %w", err)
	}

	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		merged := filepath.Join(sessionDir, e.Name(), "merged")
		if umountErr := exec.Command("umount", merged).Run(); umountErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("overlay: umount %s: %w", merged, umountErr)
			}
		}
	}

	if err := os.RemoveAll(sessionDir); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("overlay: remove session dir: %w", err)
	}
	return firstErr
}

// OverlaySupported reports whether the current platform supports overlayfs.
func OverlaySupported() bool {
	return runtime.GOOS == "linux"
}

// sanitizeName converts a container mount target like "/workspace/skills"
// into a safe directory name like "workspace-skills".
func sanitizeName(target string) string {
	result := make([]byte, 0, len(target))
	for i := 0; i < len(target); i++ {
		c := target[i]
		if c == '/' || c == '\\' {
			if len(result) > 0 {
				result = append(result, '-')
			}
			continue
		}
		result = append(result, c)
	}
	// Trim trailing separator
	for len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	if len(result) == 0 {
		return "root"
	}
	return string(result)
}
