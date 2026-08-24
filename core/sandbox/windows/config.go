//go:build windows

package windows

import (
	"os"
	"path/filepath"
)

// sandboxConfigDir is the machine/user-scoped directory holding the
// elevated sandbox's persistent state (account credentials and the
// setup marker). It deliberately lives outside any workspace: the
// sandbox account is a per-user resource, not a per-workspace one.
func sandboxConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "flowcraft", "windows-sandbox"), nil
}

func credsPath(dir string) string  { return filepath.Join(dir, "creds.json") }
func markerPath(dir string) string { return filepath.Join(dir, "setup.json") }
