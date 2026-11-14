package sandbox_test

import (
	"runtime"
	"testing"
)

// This helper was shared across the old single-package tests. The split
// keeps a copy here because a test file cannot span packages.

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sandbox-local sessions require a unix platform")
	}
}
