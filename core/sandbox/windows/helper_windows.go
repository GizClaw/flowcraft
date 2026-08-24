//go:build windows

package windows

import (
	"context"
	"fmt"
	"os"
)

// helperSecretEnv is the environment variable carrying the per-runner
// pipe secret from the unelevated runner to the re-executed elevated
// helper. It is set only for the brief launch window and removed
// immediately after, so it never leaks into later sandboxed children.
const helperSecretEnv = "FLOWCRAFT_SANDBOX_HELPER_SECRET"

// MaybeHelper runs the elevated sandbox helper when the current
// process was re-executed by the Runner for the elevated backend.
// Call it as the very first statement of the host application's main,
// before any flag parsing:
//
//	func main() {
//		if windows.MaybeHelper() {
//			return
//		}
//		// ... normal CLI startup ...
//	}
//
// The windows backend is the only caller: when an elevated spawn
// needs the helper, the Runner re-executes the running executable
// with HelperArgvMarker as argv[1] and ShellExecuteExW's runas verb
// (one UAC prompt per Runner), and this function detects that
// invocation, installs the sandbox account / WFP filters if needed,
// serves the pipe, and exits. It returns false for every ordinary
// invocation, so the host main continues normally. The host binary
// must embed this package for the elevated backend to work; without
// the hook the re-executed process would treat the marker as an
// ordinary argument.
func MaybeHelper() bool {
	if len(os.Args) < 2 || os.Args[1] != HelperArgvMarker {
		return false
	}
	code := runHelper(os.Args[2:])
	os.Exit(code)
	return true
}

// runHelper executes one helper invocation and returns the exit code.
func runHelper(args []string) int {
	parsed, err := parseHelperArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	switch parsed.mode {
	case helperModeInstall:
		if err := SandboxHelperInstall(parsed.config); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case helperModeServe:
		if err := SandboxHelperServe(context.Background(), parsed.pipe, parsed.config, parsed.root, os.Getenv(helperSecretEnv)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}
