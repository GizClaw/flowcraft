package engine

import (
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// shellArgv is the one table of shell families: the names a shell may carry, and
// the argv prefix each needs to run one command line.
//
// A shell outside this table is refused rather than half-supported, because the
// name is load-bearing twice: allowlist.go unwraps a script only for a shell it
// recognises, and the name is what a model is told the tool is. An entry here
// whose argv the approval layer does not unwrap would be a shell whose commands
// ask the approver for everything, with nothing saying why, and a name only one
// of the two tables knows is a shell configuration may not mention —
// TestShellArgvKeepsStepWithAllowlist checks both halves.
//
// The ".exe" spellings are here for Windows installs of the POSIX shells (Git
// for Windows, MSYS2): approval matches the executable's base name, and a pinned
// shell has a real path rather than a bare name.
var shellArgv = []struct {
	names []string
	args  []string
}{
	{
		names: []string{
			"sh", "bash", "zsh", "dash", "ash", "ksh", "fish",
			"sh.exe", "bash.exe", "zsh.exe", "dash.exe", "ash.exe", "ksh.exe", "fish.exe",
		},
		args: []string{"-c"},
	},
	{
		names: []string{"powershell", "pwsh"},
		args:  []string{"-NoProfile", "-NonInteractive", "-Command"},
	},
	{
		names: []string{"cmd"},
		args:  []string{"/c"},
	},
}

// DefaultShell picks the shell for one platform.
//
// Windows prefers PowerShell 7, then Windows PowerShell, then cmd; elsewhere the
// POSIX shell every backend already relies on. A missing executable falls
// through to the next candidate rather than failing construction: a host without
// pwsh still runs commands.
//
// Two Windows facts PATH alone does not carry decide the answer:
//
//   - A path under `…\WindowsApps\` is a Microsoft Store App Execution Alias or a
//     packaged install — a reparse point the activation broker resolves, not an
//     executable. exec.LookPath accepts one, but a restricted or AppContainer
//     token cannot follow it, so the child never starts and the failure reaches
//     the model as a tool error. Such an answer is rejected and the next
//     candidate is tried.
//   - A shell whose directory is not on PATH is still found at its well-known
//     install location, so a machine with PowerShell 7 installed but not on PATH
//     gets pwsh instead of silently falling back to cmd.
//
// On Windows the returned Path is the resolved absolute executable. That keeps
// two things honest: the shell a host describes and the binary that runs cannot
// disagree, and spawn time cannot re-resolve a rejected alias — a bare "pwsh"
// would go straight back through PATH, alias included, in both os/exec and the
// pseudo-console spawn.
//
// It is the answer for a host that states nothing. [ShellFor] is the other half:
// the spec for a shell the host pins itself.
//
// A caller that ships the command to another host should send Name, not Path:
// Path is this machine's executable. A caller that gates commands by exact string
// ([AllowCommands]) sees Path, so on Windows it must whitelist the absolute
// executable rather than the bare name.
//
// It lives here, not in the contract package, because it probes the host
// (exec.LookPath): the result is a fact about this machine, not about the
// sandbox contract.
func DefaultShell(goos string) sandbox.ShellSpec {
	return defaultShell(goos, hostProbe{lookPath: exec.LookPath, getenv: os.Getenv})
}

// ShellFor builds the spec for a shell the host pins instead of letting
// [DefaultShell] probe the machine — a shell whose directory PATH does not carry,
// or one whose install the probe would not choose.
//
// name is the shell, and it becomes the tool name the model sees; empty takes the
// base name of path without any ".exe" suffix, so a pinned pwsh.exe is named pwsh.
// It must be one of the shells shellArgv lists, and it must agree with path when
// the base name is a known shell: -c handed to cmd.exe runs nothing. args replaces
// the default prefix, for a build of a known shell whose flags differ; a prefix
// that is not the exact well-known form stays approval-bound, so a deployment that
// writes its own loses the auto-approval shortcut, not the boundary.
//
// The path is not stat'ed: with sandbox.backend: remote the shell runs on the far
// side, and refusing it here would refuse a deployment that works. What is refused
// is a Microsoft Store alias, which cannot be started on either side.
func ShellFor(name, path string, args []string) (sandbox.ShellSpec, error) {
	if strings.TrimSpace(path) == "" {
		return sandbox.ShellSpec{}, errdefs.Validationf("path is required")
	}
	if !usableShellPath(path) {
		return sandbox.ShellSpec{}, errdefs.Validationf(
			"path %q is under WindowsApps: a Microsoft Store App Execution Alias or a "+
				"packaged install cannot be started as a shell; name the MSI install "+
				"(%%ProgramFiles%%\\PowerShell\\7\\pwsh.exe)", path)
	}
	derived := shellBaseName(path)
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		if derived == "" {
			return sandbox.ShellSpec{}, errdefs.Validationf(
				"path %q does not name a shell executable", path)
		}
		name = derived
	}
	prefix, ok := shellArgs(name)
	if !ok {
		return sandbox.ShellSpec{}, errdefs.Validationf(
			"unknown shell %q; one of %s", name, shellNames())
	}
	if name != derived && derived != "" {
		if family, named := shellFamily(name); named {
			if other, known := shellFamily(derived); known && other != family {
				return sandbox.ShellSpec{}, errdefs.Validationf(
					"name %q contradicts path %q: the executable is %q, and the two pass a "+
						"command line differently", name, path, derived)
			}
		}
	}
	if len(args) > 0 {
		prefix = slices.Clone(args)
	}
	return sandbox.ShellSpec{Name: name, Path: path, Args: prefix}, nil
}

// hostProbe is the part of the host DefaultShell reads, injectable so the Windows
// decision table is testable on every platform instead of only on a Windows CI
// runner, where PowerShell 7 is always on PATH and the interesting cases cannot
// occur.
type hostProbe struct {
	lookPath func(string) (string, error)
	getenv   func(string) string
}

// defaultShell is DefaultShell with the host passed in.
func defaultShell(goos string, host hostProbe) sandbox.ShellSpec {
	if goos == "windows" {
		return windowsShell(host)
	}
	if _, err := host.lookPath("bash"); err == nil {
		return shellSpec("bash", "bash")
	}
	return shellSpec("sh", "/bin/sh")
}

// windowsShell resolves the Windows preference order. It does not report
// failure: cmd ships with every Windows install, and a bare "cmd" is the last
// resort when even its location cannot be probed.
func windowsShell(host hostProbe) sandbox.ShellSpec {
	for _, choice := range windowsShells(host.getenv) {
		if path, ok := choice.resolve(host); ok {
			return shellSpec(choice.name, path)
		}
	}
	return shellSpec("cmd", "cmd")
}

// shellChoice is one candidate shell and where it may be installed.
type shellChoice struct {
	name string
	// installs are the absolute locations tried after PATH, in preference order.
	installs []string
}

// windowsShells is the Windows preference order, with each candidate's
// well-known install location.
func windowsShells(getenv func(string) string) []shellChoice {
	systemRoot := getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = getenv("windir")
	}
	// ProgramW6432 is the 64-bit Program Files as seen from a 32-bit process;
	// ProgramFiles under WOW64 points at "Program Files (x86)", where a 64-bit
	// PowerShell 7 install is not.
	programFiles := getenv("ProgramW6432")
	if programFiles == "" {
		programFiles = getenv("ProgramFiles")
	}
	return []shellChoice{
		{
			name:     "pwsh",
			installs: []string{joinWindows(programFiles, `PowerShell\7\pwsh.exe`)},
		},
		{
			name:     "powershell",
			installs: []string{joinWindows(systemRoot, `System32\WindowsPowerShell\v1.0\powershell.exe`)},
		},
		{
			name:     "cmd",
			installs: []string{joinWindows(systemRoot, `System32\cmd.exe`)},
		},
	}
}

// resolve returns the executable to run for this candidate: the PATH answer when
// it is a real file, then the install locations.
func (c shellChoice) resolve(host hostProbe) (string, bool) {
	if path, err := host.lookPath(c.name); err == nil && usableShellPath(path) {
		return path, true
	}
	for _, path := range c.installs {
		if path == "" || !usableShellPath(path) {
			continue
		}
		// LookPath is the whole seam, and asking it for an absolute path is the
		// "does this file exist" probe os/exec itself uses.
		if _, err := host.lookPath(path); err == nil {
			return path, true
		}
	}
	return "", false
}

// shellSpec builds a spec for a shell this package chose: the name is one of
// shellArgv's, and the argv prefix follows from it.
func shellSpec(name, path string) sandbox.ShellSpec {
	args, _ := shellArgs(name)
	return sandbox.ShellSpec{Name: name, Path: path, Args: args}
}

// shellArgs returns the argv prefix a shell name runs one command line with, and
// whether the name is a shell this platform knows. The slice is a clone: a
// ShellSpec is a value its caller owns and may change.
func shellArgs(name string) ([]string, bool) {
	i, ok := shellFamily(name)
	if !ok {
		return nil, false
	}
	return slices.Clone(shellArgv[i].args), true
}

// shellFamily is the index of the table entry a shell name belongs to. Names in
// one family pass a command line the same way, which is what makes a stated name
// checkable against the executable's own name.
func shellFamily(name string) (int, bool) {
	for i, family := range shellArgv {
		if slices.Contains(family.names, name) {
			return i, true
		}
	}
	return 0, false
}

// shellNames lists what shellArgs accepts, in the order a reader would try them.
func shellNames() string {
	names := make([]string, 0, len(shellArgv))
	for _, family := range shellArgv {
		names = append(names, family.names...)
	}
	return strings.Join(names, ", ")
}

// shellBaseName derives a shell name from a path: the base name without the
// Windows extension, so a path ending in pwsh.exe is named pwsh.
func shellBaseName(path string) string {
	base := path
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(base), ".exe")
}

// usableShellPath reports whether a resolved path can actually be started.
//
// Windows 10/11 register App Execution Aliases under
// %LOCALAPPDATA%\Microsoft\WindowsApps — the pwsh.exe stub that opens the Store —
// and a PowerShell 7 installed from the Store lives under
// %ProgramFiles%\WindowsApps. Both are reparse points the activation broker
// resolves: exec.LookPath accepts one, but a restricted or AppContainer token
// cannot follow it. A shell that cannot be spawned is worse than the next
// candidate, so any path with a windowsapps component is rejected.
//
// The rule is on the component name alone, so a path that merely starts with it
// (`WindowsApps\pwsh.exe`) is caught too, and a directory elsewhere that happens
// to be called windowsapps is refused as well. A false refusal is a validation
// error naming the path; a false acceptance is a shell that never starts.
func usableShellPath(path string) bool {
	components := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool {
		return r == '/' || r == '\\'
	})
	for _, component := range components {
		if component == "windowsapps" {
			return false
		}
	}
	return true
}

// joinWindows joins a Windows directory and a path relative to it. It does not use
// filepath: the table it feeds is exercised on hosts whose separator is not
// Windows's, and these are Windows paths whatever host runs the test.
func joinWindows(dir, rel string) string {
	dir = strings.TrimRight(dir, `\/`)
	if dir == "" {
		return ""
	}
	return dir + `\` + rel
}
