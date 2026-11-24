package sandbox

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// The Windows paths the table below is written against. They are spelled out
// rather than derived so a case reads like a machine description.
const (
	winProgramFiles = `C:\Program Files`
	winSystemRoot   = `C:\Windows`

	winPwshInstall  = `C:\Program Files\PowerShell\7\pwsh.exe`
	winPS51Install  = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	winCmdInstall   = `C:\Windows\System32\cmd.exe`
	winPwshAlias    = `C:\Users\dev\AppData\Local\Microsoft\WindowsApps\pwsh.exe`
	winPowershAlias = `C:\Users\dev\AppData\Local\Microsoft\WindowsApps\powershell.exe`
	// winPwshPackage is the packaged install a Store copy of PowerShell 7 lands
	// in: the same class of reparse point as the alias, under a different name.
	winPwshPackage = `C:\Program Files\WindowsApps\Microsoft.PowerShell_7.4.0.0_x64__8wekyb3d8bbwe\pwsh.exe`
)

// wantArgs is the argv prefix each shell carries; resolution must not disturb it.
var wantArgs = map[string][]string{
	"pwsh":       {"-NoProfile", "-NonInteractive", "-Command"},
	"powershell": {"-NoProfile", "-NonInteractive", "-Command"},
	"cmd":        {"/c"},
	"bash":       {"-c"},
	"sh":         {"-c"},
}

// TestDefaultShellWindows pins the Windows decision table: PATH first, then the
// well-known install locations, never a Store alias, and cmd last. Every hostile
// case here is invisible to CI — a windows-latest runner keeps PowerShell 7 on
// PATH, so LookPath never reaches the alias in the first place.
func TestDefaultShellWindows(t *testing.T) {
	windowsEnv := map[string]string{"ProgramFiles": winProgramFiles, "SystemRoot": winSystemRoot}
	cases := []struct {
		name string
		// onPATH is what exec.LookPath answers for a bare name.
		onPATH map[string]string
		// installed are the absolute paths that exist on the machine.
		installed []string
		env       map[string]string
		wantName  string
		wantPath  string
	}{
		{
			name:      "pwsh on PATH wins",
			onPATH:    map[string]string{"pwsh": winPwshInstall},
			installed: []string{winPwshInstall, winPS51Install, winCmdInstall},
			env:       windowsEnv,
			wantName:  "pwsh",
			wantPath:  winPwshInstall,
		},
		{
			name:      "pwsh installed with its directory off PATH",
			installed: []string{winPwshInstall, winPS51Install, winCmdInstall},
			env:       windowsEnv,
			wantName:  "pwsh",
			wantPath:  winPwshInstall,
		},
		{
			name:      "pwsh alias rejected for the installed pwsh",
			onPATH:    map[string]string{"pwsh": winPwshAlias},
			installed: []string{winPwshInstall, winPS51Install, winCmdInstall},
			env:       windowsEnv,
			wantName:  "pwsh",
			wantPath:  winPwshInstall,
		},
		{
			name:      "pwsh alias and no pwsh 7 install falls to Windows PowerShell",
			onPATH:    map[string]string{"pwsh": winPwshAlias},
			installed: []string{winPS51Install, winCmdInstall},
			env:       windowsEnv,
			wantName:  "powershell",
			wantPath:  winPS51Install,
		},
		{
			name:      "packaged Store install rejected the same way",
			onPATH:    map[string]string{"pwsh": winPwshPackage},
			installed: []string{winPS51Install, winCmdInstall},
			env:       windowsEnv,
			wantName:  "powershell",
			wantPath:  winPS51Install,
		},
		{
			name:      "store-only machine ends on cmd",
			onPATH:    map[string]string{"pwsh": winPwshAlias, "powershell": winPowershAlias},
			installed: []string{winCmdInstall},
			env:       windowsEnv,
			wantName:  "cmd",
			wantPath:  winCmdInstall,
		},
		{
			name:      "5.1 comes from SystemRoot when ProgramFiles is unset",
			installed: []string{winPS51Install, winCmdInstall},
			env:       map[string]string{"SystemRoot": winSystemRoot},
			wantName:  "powershell",
			wantPath:  winPS51Install,
		},
		{
			name:      "windir is read when SystemRoot is absent",
			installed: []string{winPS51Install, winCmdInstall},
			env:       map[string]string{"windir": winSystemRoot},
			wantName:  "powershell",
			wantPath:  winPS51Install,
		},
		{
			// A 32-bit process on 64-bit Windows reads ProgramFiles as
			// "Program Files (x86)", where the 64-bit PowerShell 7 is not.
			name: "ProgramW6432 wins over the WOW64 ProgramFiles",
			installed: []string{
				`C:\Program Files\PowerShell\7\pwsh.exe`,
				winCmdInstall,
			},
			env: map[string]string{
				"ProgramW6432": winProgramFiles,
				"ProgramFiles": `C:\Program Files (x86)`,
				"SystemRoot":   winSystemRoot,
			},
			wantName: "pwsh",
			wantPath: `C:\Program Files\PowerShell\7\pwsh.exe`,
		},
		{
			name:     "cmd on PATH is resolved too",
			onPATH:   map[string]string{"cmd": winCmdInstall},
			env:      windowsEnv,
			wantName: "cmd",
			wantPath: winCmdInstall,
		},
		{
			name:     "nothing resolves leaves the bare cmd fallback",
			wantName: "cmd",
			wantPath: "cmd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := defaultShell("windows", fakeHost(tc.onPATH, tc.installed, tc.env))
			if got.Name != tc.wantName || got.Path != tc.wantPath {
				t.Fatalf("shell = %s %q, want %s %q", got.Name, got.Path, tc.wantName, tc.wantPath)
			}
			if !reflect.DeepEqual(got.Args, wantArgs[tc.wantName]) {
				t.Fatalf("args = %q, want %q", got.Args, wantArgs[tc.wantName])
			}
		})
	}
}

// TestDefaultShellPOSIX keeps the other branch as it was: bash when the host has
// one, sh otherwise.
func TestDefaultShellPOSIX(t *testing.T) {
	bash := defaultShell("linux", fakeHost(map[string]string{"bash": "/usr/bin/bash"}, nil, nil))
	if bash.Name != "bash" || bash.Path != "bash" {
		t.Fatalf("bash shell = %s %q, want bash \"bash\"", bash.Name, bash.Path)
	}
	if !reflect.DeepEqual(bash.Args, wantArgs["bash"]) {
		t.Fatalf("args = %q, want %q", bash.Args, wantArgs["bash"])
	}
	sh := defaultShell("linux", fakeHost(nil, nil, nil))
	if sh.Name != "sh" || sh.Path != "/bin/sh" {
		t.Fatalf("fallback shell = %s %q, want sh \"/bin/sh\"", sh.Name, sh.Path)
	}
}

// TestUsableShellPath is the rejection rule on its own: a WindowsApps segment is
// the alias or the packaged install, whatever its case and separator.
func TestUsableShellPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"pwsh install", winPwshInstall, true},
		{"windows powershell install", winPS51Install, true},
		{"cmd install", winCmdInstall, true},
		{"posix shell", "/bin/bash", true},
		{"a file named WindowsApps.exe", `C:\Tools\WindowsApps.exe`, true},
		{"a directory that only starts with WindowsApps", `C:\Tools\WindowsApps7\pwsh.exe`, true},
		{"per-user alias", winPwshAlias, false},
		{"relative path whose first component is the alias directory", `WindowsApps\pwsh.exe`, false},
		{"packaged install", winPwshPackage, false},
		{"alias in upper case", strings.ToUpper(winPwshAlias), false},
		{"alias with forward slashes", `C:/Users/dev/AppData/Local/Microsoft/WindowsApps/pwsh.exe`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usableShellPath(tc.path); got != tc.want {
				t.Fatalf("usableShellPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestDefaultShellOnThisHost is the exported entry point, not the seam: it must
// still answer for the machine running the test.
func TestDefaultShellOnThisHost(t *testing.T) {
	got := DefaultShell(runtime.GOOS)
	if got.Name == "" || got.Path == "" || len(got.Args) == 0 {
		t.Fatalf("DefaultShell(%q) = %+v, want a named shell with argv", runtime.GOOS, got)
	}
	if !usableShellPath(got.Path) {
		t.Fatalf("DefaultShell(%q).Path = %q, which cannot be started", runtime.GOOS, got.Path)
	}
	if _, err := exec.LookPath(got.Path); err != nil {
		t.Fatalf("DefaultShell(%q).Path = %q: %v", runtime.GOOS, got.Path, err)
	}
	// The Windows half of the contract: a probed shell is an absolute executable,
	// so nothing re-resolves a rejected alias at spawn time. Without this the
	// whole decision table could regress to bare names and stay green, because
	// every other test injects the probe.
	if runtime.GOOS == "windows" && !filepath.IsAbs(got.Path) {
		t.Fatalf("DefaultShell(windows).Path = %q, want an absolute executable", got.Path)
	}
}

// TestShellFor covers the pinned half: a host names the executable, the name and
// the argv prefix follow from it unless it states them.
func TestShellFor(t *testing.T) {
	cases := []struct {
		label    string
		name     string
		path     string
		args     []string
		wantName string
		wantArgs []string
		wantErr  string
	}{
		{
			label:    "name derived from a Windows path",
			path:     winPwshInstall,
			wantName: "pwsh",
			wantArgs: wantArgs["pwsh"],
		},
		{
			label:    "name derived from a POSIX path",
			path:     "/usr/local/bin/bash",
			wantName: "bash",
			wantArgs: wantArgs["bash"],
		},
		{
			label:    "extension is dropped whatever its case",
			path:     `C:\tools\PWSh.EXE`,
			wantName: "pwsh",
			wantArgs: wantArgs["pwsh"],
		},
		{
			label:    "stated name wins over the base name",
			name:     "sh",
			path:     "/usr/local/bin/zsh",
			wantName: "sh",
			wantArgs: wantArgs["sh"],
		},
		{
			label:    "stated args replace the default prefix",
			name:     "bash",
			path:     "/bin/bash",
			args:     []string{"-lc"},
			wantName: "bash",
			wantArgs: []string{"-lc"},
		},
		{
			label:    "a bare name passes through untouched",
			path:     "pwsh",
			wantName: "pwsh",
			wantArgs: wantArgs["pwsh"],
		},
		{
			label:    "a stated name is case-folded like a derived one",
			name:     "PowerShell",
			path:     winPS51Install,
			wantName: "powershell",
			wantArgs: wantArgs["powershell"],
		},
		{
			label:   "empty path",
			name:    "bash",
			wantErr: "path is required",
		},
		{
			label:   "blank path",
			name:    "bash",
			path:    "   ",
			wantErr: "path is required",
		},
		{
			label:   "a path with no base name",
			path:    "/usr/local/bin/bash/",
			wantErr: "does not name a shell executable",
		},
		{
			label:   "a Store alias is refused however it is named",
			path:    winPwshAlias,
			wantErr: "WindowsApps",
		},
		{
			label:   "an unknown shell is refused with the known names",
			path:    "/usr/local/bin/nu",
			wantErr: "unknown shell \"nu\"",
		},
		{
			label:   "a stated name that contradicts the executable is refused",
			name:    "bash",
			path:    winCmdInstall,
			wantErr: "contradicts path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got, err := ShellFor(tc.name, tc.path, tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ShellFor(%q, %q) = %+v, want an error", tc.name, tc.path, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ShellFor(%q, %q): %v", tc.name, tc.path, err)
			}
			if got.Name != tc.wantName || got.Path != tc.path {
				t.Fatalf("spec = %s %q, want %s %q", got.Name, got.Path, tc.wantName, tc.path)
			}
			if !reflect.DeepEqual(got.Args, tc.wantArgs) {
				t.Fatalf("args = %q, want %q", got.Args, tc.wantArgs)
			}
		})
	}
}

// TestShellSpecsDoNotShareArgv pins the clone: two specs built from the same table
// entry are separate values, so a caller editing one cannot change the shell every
// other tool in the process runs.
func TestShellSpecsDoNotShareArgv(t *testing.T) {
	first, err := ShellFor("bash", "/bin/bash", nil)
	if err != nil {
		t.Fatalf("ShellFor: %v", err)
	}
	first.Args[0] = "-lc"
	second := shellSpec("bash", "/bin/bash")
	if !reflect.DeepEqual(second.Args, wantArgs["bash"]) {
		t.Fatalf("second spec args = %q, want %q", second.Args, wantArgs["bash"])
	}
	// A caller's own args are cloned too: editing the slice it passed in must not
	// reach the spec it got back.
	args := []string{"-lc"}
	pinned, err := ShellFor("bash", "/bin/bash", args)
	if err != nil {
		t.Fatalf("ShellFor: %v", err)
	}
	args[0] = "-c"
	if !reflect.DeepEqual(pinned.Args, []string{"-lc"}) {
		t.Fatalf("spec args = %q, want the value the caller wrote", pinned.Args)
	}
}

// TestShellArgvKeepsStepWithAllowlist is the invariant between the two shell
// tables, on both halves. Names: every name the approval layer unwraps has an
// argv prefix to run with, and every name this package offers is one the approval
// layer can classify. Argv: the prefix this package emits is the form the
// approval layer unwraps. Either half out of step is silent — a shell whose every
// command asks the approver, or a name no configuration may use — and nothing in
// the failure says why.

// fakeHost models a machine: onPATH is what exec.LookPath answers for a bare
// name, installed lists the absolute paths that exist, and env is the process
// environment — matched case-insensitively, as Windows reads it.
func fakeHost(onPATH map[string]string, installed []string, env map[string]string) hostProbe {
	exists := make(map[string]bool, len(installed))
	for _, path := range installed {
		exists[path] = true
	}
	return hostProbe{
		lookPath: func(name string) (string, error) {
			if path, ok := onPATH[name]; ok {
				return path, nil
			}
			if exists[name] {
				return name, nil
			}
			return "", exec.ErrNotFound
		},
		getenv: func(key string) string {
			for name, value := range env {
				if strings.EqualFold(name, key) {
					return value
				}
			}
			return ""
		},
	}
}
