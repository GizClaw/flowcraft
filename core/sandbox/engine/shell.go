package engine

import (
	"os/exec"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// DefaultShell picks the shell for one platform.
//
// Windows prefers PowerShell 7, then Windows PowerShell, then cmd; elsewhere the
// POSIX shell every backend already relies on. A missing executable falls
// through to the next candidate rather than failing construction: a host without
// pwsh still runs commands.
//
// It lives here, not in the contract package, because it probes the host
// (exec.LookPath): the result is a fact about this machine, not about the
// sandbox contract.
func DefaultShell(goos string) sandbox.ShellSpec {
	if goos == "windows" {
		for _, candidate := range []sandbox.ShellSpec{
			{Name: "pwsh", Path: "pwsh", Args: []string{"-NoProfile", "-NonInteractive", "-Command"}},
			{Name: "powershell", Path: "powershell", Args: []string{"-NoProfile", "-NonInteractive", "-Command"}},
			{Name: "cmd", Path: "cmd", Args: []string{"/c"}},
		} {
			if _, err := exec.LookPath(candidate.Path); err == nil {
				return candidate
			}
		}
		return sandbox.ShellSpec{Name: "cmd", Path: "cmd", Args: []string{"/c"}}
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return sandbox.ShellSpec{Name: "bash", Path: "bash", Args: []string{"-c"}}
	}
	return sandbox.ShellSpec{Name: "sh", Path: "/bin/sh", Args: []string{"-c"}}
}
