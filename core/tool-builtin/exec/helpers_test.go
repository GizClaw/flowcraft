package exec

import (
	goruntime "runtime"
	"strings"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
)

// testShell is the shell these tests write their command lines for, pinned
// instead of discovered: DefaultShell picks whichever of pwsh / powershell / cmd
// a Windows host happens to have, and the three do not agree on syntax.
func testShell() sandbox.ShellSpec {
	if goruntime.GOOS == "windows" {
		return sandbox.ShellSpec{Name: "cmd", Path: "cmd", Args: []string{"/c"}}
	}
	return sandbox.ShellSpec{Name: "sh", Path: "/bin/sh", Args: []string{"-c"}}
}

// longRunningArgv is a command that stays alive until it is terminated, rendered
// for the platform: the tests assert session behaviour, not one shell's spelling.
func longRunningArgv() []string {
	shell := sandboxtest.ForPlatform(goruntime.GOOS)
	return shell.Argv(shell.LongRunning())
}

// stdinReaderArgv is a program that copies stdin to stdout and exits at EOF,
// which is how a test proves the input side of a session.
func stdinReaderArgv() []string {
	return sandboxtest.ForPlatform(goruntime.GOOS).StdinReader()
}

// bigOutputCommand writes more than the inline cap (1 MiB) so the truncation
// path runs: a loop is the portable way to say "a lot of bytes", and each shell
// spells it differently.
func bigOutputCommand() string {
	line := strings.Repeat("x", 100)
	if goruntime.GOOS == "windows" {
		return `for /L %i in (1,1,11000) do @echo ` + line
	}
	return `i=0; while [ $i -lt 11000 ]; do echo ` + line + `; i=$((i+1)); done`
}
