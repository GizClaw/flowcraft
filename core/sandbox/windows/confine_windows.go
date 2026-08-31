//go:build windows

package windows

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	xwin "golang.org/x/sys/windows"
)

// labelLowIntegrity sets the mandatory integrity label of path (and
// every existing child, recursively) to Low with NO_WRITE_UP. A
// low-IL child can then write anywhere under the root while the rest
// of the host stays Medium (readable, not writable). Setting the
// mandatory label needs no special privilege: LABEL_SECURITY_INFORMATION
// is distinct from SACL_SECURITY_INFORMATION (which would require
// SeSecurityPrivilege).
func labelLowIntegrity(path string) error {
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var flags uint32
		if d.IsDir() {
			// New children inherit the Low label.
			flags = xwin.OBJECT_INHERIT_ACE | xwin.CONTAINER_INHERIT_ACE
		}
		return labelLowIntegrityOne(p, flags)
	})
}

func labelLowIntegrityOne(path string, aceFlags uint32) error {
	lowSID, err := lowIntegritySID()
	if err != nil {
		return err
	}
	var acl xwin.ACL
	if err := initializeAcl(&acl, 256); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: init label acl: %w", err))
	}
	if err := addMandatoryAce(&acl, aceFlags, systemMandatoryLabelNoWriteUp, lowSID); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: add mandatory ace for %s: %w", path, err))
	}
	if err := xwin.SetNamedSecurityInfo(path, xwin.SE_FILE_OBJECT,
		xwin.LABEL_SECURITY_INFORMATION, nil, nil, nil, &acl); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: set low integrity label on %s: %w", path, err))
	}
	return nil
}

// createLowTempDir creates a per-runner Low-labeled scratch directory
// that the sandboxed child gets as its TEMP/TMP. The user's Medium
// temp is write-denied for a Low subject, and most tooling expects a
// writable temp, so confinement must provide one.
func createLowTempDir() (string, error) {
	dir, err := os.MkdirTemp("", "flowcraft-low-")
	if err != nil {
		return "", errdefs.Internal(fmt.Errorf("windows: create low-IL temp dir: %w", err))
	}
	if err := labelLowIntegrity(dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// withLowTempEnv replaces TEMP/TMP/TMPDIR in env with the low-IL
// scratch dir so the sandboxed child has a writable temp.
func withLowTempEnv(env []string, temp string) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && (key == "TEMP" || key == "TMP" || key == "TMPDIR") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TEMP="+temp, "TMP="+temp, "TMPDIR="+temp)
}
