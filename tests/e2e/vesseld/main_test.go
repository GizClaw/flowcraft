//go:build e2e

package vesseld_e2e

import (
	"fmt"
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestMain front-loads the vesseld build so the first test does
// not pay a ~1s linker hit. Subsequent helpers.EnsureBinary
// calls hit the sync.Once fast path. We intentionally do NOT
// remove the binary here — the OS temp cleanup handles it, and
// keeping it around lets a developer re-run a single test with
// `go test -run X` and reuse the artifact.
func TestMain(m *testing.M) {
	if _, err := helpers.EnsureBinaryNoT(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: prebuild vesseld failed: %v\n", err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}
