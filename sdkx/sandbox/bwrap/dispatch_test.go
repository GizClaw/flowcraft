//go:build linux

package bwrap

import (
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/sdkx/sandbox/bwrap/internal/bridge"
)

// TestMain lets the test binary double as the in-netns bridge: the
// bwrap runner re-executes the running executable (the test binary
// here) with bridge.Marker as argv[1], and this hook hands control to
// the bridge implementation so NetAllowList / NetProxy integration
// cases can run without compiling a separate bridge binary.
func TestMain(m *testing.M) {
	if bridge.MaybeRun() {
		return // bridge already os.Exit'ed with the proxied command's code
	}
	os.Exit(m.Run())
}
