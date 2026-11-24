package sandbox_test

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// TestShellSpecArgv pins the shape a command line takes: the shell's prefix
// followed by the whole line as one argument. tool-builtin/exec builds argv from
// this, so a change here changes every command the platform runs.
func TestShellSpecArgv(t *testing.T) {
	for name, tc := range map[string]struct {
		spec    sandbox.ShellSpec
		command string
		want    []string
	}{
		"posix": {
			spec:    sandbox.ShellSpec{Name: "sh", Path: "/bin/sh", Args: []string{"-c"}},
			command: "echo hi",
			want:    []string{"-c", "echo hi"},
		},
		"powershell": {
			spec:    sandbox.ShellSpec{Name: "pwsh", Args: []string{"-NoProfile", "-Command"}},
			command: "Get-Date",
			want:    []string{"-NoProfile", "-Command", "Get-Date"},
		},
		"no prefix": {
			spec:    sandbox.ShellSpec{Name: "custom"},
			command: "run this",
			want:    []string{"run this"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.spec.Argv(tc.command); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Argv(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

// TestShellSpecArgvDoesNotAlias is the bug this shape avoids: appending to the
// spec's own slice would rewrite the shell's prefix for every later command in the
// process, and the mutation would be invisible at the call site.
func TestShellSpecArgvDoesNotAlias(t *testing.T) {
	spec := sandbox.ShellSpec{Name: "sh", Args: []string{"-c"}}
	argv := spec.Argv("first")
	argv[0] = "-broken"

	if spec.Args[0] != "-c" {
		t.Fatalf("Argv aliased the spec's Args: %v", spec.Args)
	}
	if got := spec.Argv("second"); !reflect.DeepEqual(got, []string{"-c", "second"}) {
		t.Fatalf("a later Argv call = %v", got)
	}
}

func TestShellSpecCommandLine(t *testing.T) {
	spec := sandbox.ShellSpec{Name: "pwsh", Path: `C:\pwsh.exe`}
	if got := spec.CommandLine(); got != "pwsh" {
		t.Fatalf("CommandLine() = %q, want the shell name a model is told", got)
	}
}
