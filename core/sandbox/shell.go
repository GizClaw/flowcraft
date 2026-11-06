package sandbox

import "os/exec"

// ShellSpec names the shell a tool uses to run a command line, and how to build
// its argv.
//
// It lives here rather than in a tool package because the choice is a platform
// fact, the same kind of fact the backends already encode: a command line needs
// a shell, and which shell exists depends on the host.
type ShellSpec struct {
	// Name is how the shell is described to a model ("sh", "pwsh").
	Name string
	// Path is the executable.
	Path string
	// Args are the arguments that precede the command line, e.g. "-c".
	Args []string
}

// CommandLine renders the shell for a model-facing description.
func (s ShellSpec) CommandLine() string { return s.Name }

// Argv builds the full argv for one command line.
func (s ShellSpec) Argv(command string) []string {
	argv := make([]string, 0, len(s.Args)+1)
	argv = append(argv, s.Args...)
	return append(argv, command)
}

// DefaultShell picks the shell for one platform.
//
// Windows prefers PowerShell 7, then Windows PowerShell, then cmd; elsewhere the
// POSIX shell every backend already relies on. A missing executable falls
// through to the next candidate rather than failing construction: a host without
// pwsh still runs commands.
func DefaultShell(goos string) ShellSpec {
	if goos == "windows" {
		for _, candidate := range []ShellSpec{
			{Name: "pwsh", Path: "pwsh", Args: []string{"-NoProfile", "-NonInteractive", "-Command"}},
			{Name: "powershell", Path: "powershell", Args: []string{"-NoProfile", "-NonInteractive", "-Command"}},
			{Name: "cmd", Path: "cmd", Args: []string{"/c"}},
		} {
			if _, err := exec.LookPath(candidate.Path); err == nil {
				return candidate
			}
		}
		return ShellSpec{Name: "cmd", Path: "cmd", Args: []string{"/c"}}
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return ShellSpec{Name: "bash", Path: "bash", Args: []string{"-c"}}
	}
	return ShellSpec{Name: "sh", Path: "/bin/sh", Args: []string{"-c"}}
}
