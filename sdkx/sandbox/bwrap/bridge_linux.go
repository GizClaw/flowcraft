//go:build linux

package bwrap

import "github.com/GizClaw/flowcraft/sdkx/sandbox/bwrap/internal/bridge"

// MaybeBridge runs the in-netns bridge when the current process was
// re-executed by the bwrap runner for NetAllowList / NetProxy execs.
// Call it as the very first statement of the host application's main,
// before any flag parsing:
//
//	func main() {
//		if bwrap.MaybeBridge() {
//			return
//		}
//		// ... normal CLI startup ...
//	}
//
// The bwrap backend is the only caller: when a NetAllowList /
// NetProxy Exec needs a bridge, the runner re-executes the running
// executable with the reserved marker argument (bridge.Marker), and
// this function detects that invocation, serves the in-netns
// loopback-to-UDS gate, and exits with the proxied command's status.
// It returns false for every ordinary invocation, so the host main
// continues normally. The host binary must embed this package (or
// sdkx/sandbox/bwrap/internal/bridge) for network-enforced modes to
// work; without the hook the re-executed process would treat the
// marker as an ordinary argument.
func MaybeBridge() bool {
	return bridge.MaybeRun()
}
