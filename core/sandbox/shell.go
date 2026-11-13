package sandbox

// ShellSpec names the shell a tool uses to run a command line, and how to build
// its argv.
//
// It lives here rather than in a tool package because the choice is a platform
// fact, the same kind of fact the backends already encode: a command line needs
// a shell, and which shell exists depends on the host.
//
// Picking one — [DefaultShell] — is implementation, not contract: it probes the
// host with exec.LookPath, so it lives in the engine package with the rest of
// the behavior. This package stays free of OS probing.
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
