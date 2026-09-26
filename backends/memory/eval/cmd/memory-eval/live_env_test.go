package main

import (
	"os"
	"path/filepath"
	"testing"
)

// liveEnvFile resolves the credentials file the live lanes need. The README
// tells operators to keep it at the repository root, while earlier runs put it
// next to the eval module; both are accepted so following the docs does not
// silently skip every credentialed test.
func liveEnvFile(t *testing.T) string {
	t.Helper()
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	candidates := []string{
		filepath.Join(workdir, "..", "..", ".env"),
		filepath.Join(workdir, "..", "..", "..", "..", "..", ".env"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skipf("no credentials file in %v", candidates)
	return ""
}
