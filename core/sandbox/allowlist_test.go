package sandbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

func TestAllowlistRejectsInvalidRules(t *testing.T) {
	for _, rule := range []string{"", "*", "go r*n", "go run * extra", "  "} {
		if _, err := sandbox.NewAllowlist(rule); err == nil {
			t.Fatalf("NewAllowlist(%q) succeeded, want error", rule)
		}
	}
	if _, err := sandbox.NewAllowlist("go *"); err != nil {
		t.Fatalf("NewAllowlist(go *) error = %v", err)
	}
}

func TestAllowlistMatches(t *testing.T) {
	a, err := sandbox.NewAllowlist("go *", "go run *", "git status", "/usr/bin/git *")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  sandbox.ExecRequest
		want bool
	}{
		{"bare go", sandbox.ExecRequest{Command: "go"}, true},
		{"go build", sandbox.ExecRequest{Command: "go", Args: []string{"build", "x"}}, true},
		{"absolute go by basename", sandbox.ExecRequest{Command: "/usr/bin/go", Args: []string{"mod", "tidy"}}, true},
		{"go run", sandbox.ExecRequest{Command: "go", Args: []string{"run"}}, true},
		{"go run main", sandbox.ExecRequest{Command: "go", Args: []string{"run", "main.go"}}, true},
		{"other program", sandbox.ExecRequest{Command: "python3"}, false},
		{"exact git status", sandbox.ExecRequest{Command: "git", Args: []string{"status"}}, true},
		{"git status with args", sandbox.ExecRequest{Command: "git", Args: []string{"status", "-s"}}, false},
		{"bare git", sandbox.ExecRequest{Command: "git"}, false},
		{"slash rule literal", sandbox.ExecRequest{Command: "/usr/bin/git", Args: []string{"log"}}, true},
		{"slash rule not basename", sandbox.ExecRequest{Command: "git", Args: []string{"log"}}, false},
	}
	for _, tc := range cases {
		if got := a.Matches(tc.req); got != tc.want {
			t.Errorf("%s: Matches(%+v) = %v, want %v", tc.name, tc.req, got, tc.want)
		}
	}
}

func TestNormaliseExecUnwrapsShell(t *testing.T) {
	tokens := sandbox.NormaliseExec(sandbox.ExecRequest{
		Command: "sh", Args: []string{"-c", "go run main.go"},
	})
	if want := []string{"go", "run", "main.go"}; !reflect.DeepEqual(tokens, want) {
		t.Fatalf("NormaliseExec(sh -c) = %v, want %v", tokens, want)
	}
	tokens = sandbox.NormaliseExec(sandbox.ExecRequest{
		Command: "git", Args: []string{"status"},
	})
	if want := []string{"git", "status"}; !reflect.DeepEqual(tokens, want) {
		t.Fatalf("NormaliseExec(git status) = %v, want %v", tokens, want)
	}
	tokens = sandbox.NormaliseExec(sandbox.ExecRequest{
		Command: "/bin/sh", Args: []string{"-c", "FOO=1 python3 script.py"},
	})
	if want := []string{"python3", "script.py"}; !reflect.DeepEqual(tokens, want) {
		t.Fatalf("NormaliseExec(env-prefixed) = %v, want %v", tokens, want)
	}
}

func TestAllowlistMatchesShellInvocation(t *testing.T) {
	a, err := sandbox.NewAllowlist("go *", "python3 *", "ls *", "echo *")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  sandbox.ExecRequest
		want bool
	}{
		{"sh -c simple", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "go run main.go"}}, true},
		{"abs sh -c", sandbox.ExecRequest{Command: "/bin/sh", Args: []string{"-c", "python3 script.py"}}, true},
		{"bash -lc not unwrapped", sandbox.ExecRequest{Command: "bash", Args: []string{"-lc", "ls -la"}}, false},
		{"quoted arg", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", `echo 'a b'`}}, true},
		{"env prefix", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "FOO=1 go test ./..."}}, true},
		{"unsafe PATH assignment", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "PATH=/evil ls -la"}}, false},
		{"unsafe git env config", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.external GIT_CONFIG_VALUE_0=x git diff"}}, false},
		{"command chain denied", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "npm install && rm -rf /"}}, false},
		{"pipe denied", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "go list | grep x"}}, false},
		{"substitution denied", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "go run $(echo x)"}}, false},
		{"unterminated quote denied", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", `echo "oops`}}, false},
	}
	for _, tc := range cases {
		if got := a.Matches(tc.req); got != tc.want {
			t.Errorf("%s: Matches(%+v) = %v, want %v", tc.name, tc.req, got, tc.want)
		}
	}
}

func TestAllowlistMatchesWindowsShellInvocations(t *testing.T) {
	a, err := sandbox.NewAllowlist("git status", "go *", "dir *")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  sandbox.ExecRequest
		want bool
	}{
		{
			"cmd exact rule",
			sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "git status"}},
			true,
		},
		{
			"cmd uppercase switch",
			sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/C", "git status"}},
			true,
		},
		{
			"cmd wildcard rule",
			sandbox.ExecRequest{Command: "cmd", Args: []string{"/c", "go test ./..."}},
			true,
		},
		{
			"cmd native wildcard keeps the rule shape",
			sandbox.ExecRequest{Command: "cmd", Args: []string{"/c", "dir *.go"}},
			true,
		},
		{
			"cmd exact rule with extra args",
			sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "git status --short"}},
			false,
		},
		{
			"cmd /k stays raw",
			sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/k", "git status"}},
			false,
		},
		{
			"cmd chain stays raw",
			sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "git status && del x"}},
			false,
		},
		{
			"pwsh exact pair",
			sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-Command", "git status"}},
			true,
		},
		{
			"powershell.exe case-folded",
			sandbox.ExecRequest{Command: "PowerShell.exe", Args: []string{"-nopROFILE", "-command", "go test ./..."}},
			true,
		},
		{
			"pwsh without -NoProfile stays raw",
			sandbox.ExecRequest{Command: "pwsh", Args: []string{"-Command", "git status"}},
			false,
		},
		{
			"posix sh still unwraps",
			sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "git status"}},
			true,
		},
	}
	for _, tc := range cases {
		if got := a.Matches(tc.req); got != tc.want {
			t.Errorf("%s: Matches(%+v) = %v, want %v", tc.name, tc.req, got, tc.want)
		}
	}
}

func TestNormaliseExecUnwrapsWindowsShells(t *testing.T) {
	tests := []struct {
		name string
		req  sandbox.ExecRequest
		want []string
	}{
		{
			"cmd bare switch",
			sandbox.ExecRequest{Command: "cmd", Args: []string{"/c", "git status"}},
			[]string{"git", "status"},
		},
		{
			"cmd.exe uppercase switch",
			sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/C", "go run main.go"}},
			[]string{"go", "run", "main.go"},
		},
		{
			"cmd.exe case-folded name",
			sandbox.ExecRequest{Command: "CMD.EXE", Args: []string{"/c", "dir /b"}},
			[]string{"dir", "/b"},
		},
		{
			"cmd wildcard words stay plain",
			sandbox.ExecRequest{Command: "cmd", Args: []string{"/c", "dir *.go a?b"}},
			[]string{"dir", "*.go", "a?b"},
		},
		{
			"cmd non-ascii words",
			sandbox.ExecRequest{Command: "cmd", Args: []string{"/c", "type 日志.txt"}},
			[]string{"type", "日志.txt"},
		},
		{
			"pwsh exact pair",
			sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-Command", "git status"}},
			[]string{"git", "status"},
		},
		{
			"powershell.exe case-folded",
			sandbox.ExecRequest{Command: "PowerShell.exe", Args: []string{"-nopROFILE", "-command", "go test ./..."}},
			[]string{"go", "test", "./..."},
		},
		{
			"pwsh native path with backslash",
			sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-Command", `type C:\tmp\note.txt`}},
			[]string{"type", `C:\tmp\note.txt`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandbox.NormaliseExec(tc.req); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NormaliseExec(%+v) = %v, want %v", tc.req, got, tc.want)
			}
		})
	}
}

// TestNormaliseExecKeepsUnprovableWrappersRaw pins the fail-closed side
// of the shell profiles: every wrapper form and script character the
// profile does not model stays raw, so it can never match a rule.
func TestNormaliseExecKeepsUnprovableWrappersRaw(t *testing.T) {
	tests := []struct {
		name string
		req  sandbox.ExecRequest
	}{
		{"cmd keep-prompt switch", sandbox.ExecRequest{Command: "cmd", Args: []string{"/k", "git status"}}},
		{"cmd quote-rewrite switch", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/s", "/c", "git status"}}},
		{"cmd delayed expansion", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/v:on", "/c", "echo !PATH!"}}},
		{"cmd extra argv", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "dir", `C:\tmp`}}},
		{"cmd chain", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "npm install && del /s /q build"}}},
		{"cmd pipe", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "dir | findstr go"}}},
		{"cmd redirect", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "echo x > out.txt"}}},
		{"cmd caret escape", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "echo ^& calc"}}},
		{"cmd percent expansion", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "echo %PATH%"}}},
		{"cmd double quotes", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", `echo "a b"`}}},
		{"cmd single quotes", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "echo 'a b'"}}},
		{"cmd backslash path", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", `dir C:\tmp\*.go`}}},
		{"cmd newline", sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", "dir\ndel x"}}},
		{"pwsh without -NoProfile", sandbox.ExecRequest{Command: "pwsh", Args: []string{"-Command", "git status"}}},
		{"pwsh abbreviated params", sandbox.ExecRequest{Command: "pwsh", Args: []string{"-nop", "-c", "git status"}}},
		{"pwsh encoded command", sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-EncodedCommand", "ZwBpAHQAIABzAHQAYQB0AHUAcwA="}}},
		{"pwsh statement separator", sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-Command", "git status; Remove-Item x"}}},
		{"pwsh variable", sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-Command", "git $env:REF"}}},
		{"pwsh subexpression", sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-Command", "git $(Get-Date)"}}},
		{"pwsh association", sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-Command", "git -c a=b status"}}},
		{"pwsh multiple script argv", sandbox.ExecRequest{Command: "pwsh", Args: []string{"-NoProfile", "-Command", "git", "status"}}},
		{"posix login shell", sandbox.ExecRequest{Command: "bash", Args: []string{"-lc", "ls -la"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := append([]string{tc.req.Command}, tc.req.Args...)
			if got := sandbox.NormaliseExec(tc.req); !reflect.DeepEqual(got, want) {
				t.Fatalf("NormaliseExec(%+v) = %v, want raw %v", tc.req, got, want)
			}
		})
	}
}

// TestNormaliseExecWindowsAbsoluteShellPath documents that the shell
// name check uses filepath.Base, so an absolute Windows path is only
// split on the windows lane. Splitting on '\' everywhere would
// over-match POSIX names that contain a backslash, so the behaviour is
// deliberately platform-specific.
func TestNormaliseExecWindowsAbsoluteShellPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("filepath.Base treats '\\' as a separator on Windows only")
	}
	req := sandbox.ExecRequest{
		Command: `C:\Windows\System32\cmd.exe`,
		Args:    []string{"/c", "git status"},
	}
	if got, want := sandbox.NormaliseExec(req), []string{"git", "status"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NormaliseExec(%+v) = %v, want %v", req, got, want)
	}
}

// TestShellArgvWitnessHelper is not a test: it prints its own argv as
// JSON so the witness tests below can observe what a real shell hands
// to a program. The staged test binary only takes this path when the
// witness environment variable is set.
func TestShellArgvWitnessHelper(t *testing.T) {
	if os.Getenv(witnessEnv) != "1" {
		return
	}
	payload, err := json.Marshal(os.Args)
	if err != nil {
		fmt.Println(witnessMarker + "null")
		os.Exit(1)
	}
	fmt.Println(witnessMarker + string(payload))
	os.Exit(0)
}

const (
	witnessEnv    = "FLOWCRAFT_SANDBOX_ARGV_WITNESS"
	witnessMarker = "argv-witness:"
)

// TestUnwrapMatchesRealPosixShell is the differential witness for the
// sh profile: whenever NormaliseExec unwraps a script, running that
// script under the real shell must hand the program exactly the
// promised tokens.
func TestUnwrapMatchesRealPosixShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX witness runs on the unix lanes")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	helper := "./" + filepath.Base(exe)
	for _, extra := range [][]string{
		nil,
		{"a"},
		{"a", "b"},
		{"-x", "--long", "v=1"},
	} {
		script := strings.Join(
			append([]string{helper, "-test.run=TestShellArgvWitnessHelper", "--"}, extra...),
			" ",
		)
		req := sandbox.ExecRequest{Command: "sh", Args: []string{"-c", script}}
		want := sandbox.NormaliseExec(req)
		if len(want) == 0 || want[0] != helper {
			t.Skipf("script %q is outside the plain-word gate (%v)", script, want)
		}
		if got := witnessArgv(t, filepath.Dir(exe), "sh", "-c", script); !reflect.DeepEqual(got, want) {
			t.Errorf("script %q: shell argv = %v, NormaliseExec = %v", script, got, want)
		}
	}
}

// TestUnwrapMatchesRealCmd is the same witness for the cmd profile. It
// stages the test binary as a standalone program so cmd can run it
// through the same plain-word script the profile claims to prove.
func TestUnwrapMatchesRealCmd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the cmd witness runs on the windows lane")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	if err := copyFile(exe, filepath.Join(dir, "argvdump.exe")); err != nil {
		t.Skipf("stage witness binary: %v", err)
	}
	for _, extra := range [][]string{
		nil,
		{"a", "b"},
		{"-x", "--long", "v=1"},
		{"*.go", "a?b"},
	} {
		script := strings.Join(
			append([]string{"argvdump.exe", "-test.run=TestShellArgvWitnessHelper", "--"}, extra...),
			" ",
		)
		req := sandbox.ExecRequest{Command: "cmd.exe", Args: []string{"/c", script}}
		want := sandbox.NormaliseExec(req)
		if len(want) == 0 || want[0] != "argvdump.exe" {
			t.Skipf("script %q is outside the plain-word gate (%v)", script, want)
		}
		if got := witnessArgv(t, dir, "cmd.exe", "/c", script); !reflect.DeepEqual(got, want) {
			t.Errorf("script %q: cmd argv = %v, NormaliseExec = %v", script, got, want)
		}
	}
}

// witnessArgv runs program through the real shell and returns the argv
// the witness helper observed.
func witnessArgv(t *testing.T, dir, program string, args ...string) []string {
	t.Helper()
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), witnessEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", program, args, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(line, witnessMarker)
		if !ok {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(rest), &argv); err != nil {
			t.Fatalf("witness output %q: %v", rest, err)
		}
		return argv
	}
	t.Fatalf("%s %v: witness marker missing in output %q", program, args, out)
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

func TestAllowlistMutationAndUnion(t *testing.T) {
	a, err := sandbox.NewAllowlist("ls *")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Add("go *", "ls *"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ls *", "go *"}; !reflect.DeepEqual(a.Rules(), want) {
		t.Fatalf("Rules() = %v, want %v", a.Rules(), want)
	}

	extra, err := sandbox.NewAllowlist("git status", "go *")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Union(extra); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ls *", "go *", "git status"}; !reflect.DeepEqual(a.Rules(), want) {
		t.Fatalf("Rules() after Union = %v, want %v", a.Rules(), want)
	}

	if err := a.Set([]string{"python3 *"}); err != nil {
		t.Fatal(err)
	}
	if a.Matches(sandbox.ExecRequest{Command: "go"}) {
		t.Fatal("Set did not replace rules: go still matches")
	}
	if !a.Matches(sandbox.ExecRequest{Command: "python3", Args: []string{"x.py"}}) {
		t.Fatal("Set did not apply replacement: python3 does not match")
	}
	if err := a.Add("bad rule * inside"); err == nil {
		t.Fatal("Add applied an invalid rule")
	}
	if want := []string{"python3 *"}; !reflect.DeepEqual(a.Rules(), want) {
		t.Fatalf("Rules() after failed Add = %v, want %v", a.Rules(), want)
	}
}

func TestAllowlistConcurrentAddAndMatch(t *testing.T) {
	a, err := sandbox.NewAllowlist("go *")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = a.Add(fmt.Sprintf("tool-%d *", i))
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if !a.Matches(sandbox.ExecRequest{Command: "go", Args: []string{"build"}}) {
					t.Error("allowlist lost the base rule during concurrent Add")
				}
			}
		}()
	}
	wg.Wait()
	if got := len(a.Rules()); got != 101 {
		t.Fatalf("rules = %d, want 101 (idempotent Add)", got)
	}
}

func TestAllowlistNotAllowedPredicate(t *testing.T) {
	a, err := sandbox.NewAllowlist("go *")
	if err != nil {
		t.Fatal(err)
	}
	p := a.NotAllowed()
	if reason, matched := p.Match(sandbox.ExecRequest{Command: "go"}); matched {
		t.Fatalf("NotAllowed matched an allowlisted command: %s", reason)
	}
	if reason, matched := p.Match(sandbox.ExecRequest{Command: "rm", Args: []string{"-rf", "/"}}); !matched || reason == "" {
		t.Fatalf("NotAllowed did not match an out-of-bounds command: %q, %v", reason, matched)
	}
	var nilList *sandbox.Allowlist
	if _, matched := nilList.NotAllowed().Match(sandbox.ExecRequest{Command: "anything"}); !matched {
		t.Fatal("nil allowlist NotAllowed must match every command (fail closed)")
	}
}

func TestWithApprovalUsesAllowlist(t *testing.T) {
	skipOnWindows(t)
	inner := localRunner(t)
	var approvals atomic.Int64
	approve := sandbox.ApprovalFunc(func(context.Context, sandbox.ApprovalRequest) (sandbox.Decision, error) {
		approvals.Add(1)
		return sandbox.Allow, nil
	})
	allowlist, err := sandbox.NewAllowlist("echo *")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// In-bounds call: allowlist pre-approves, approver is never asked.
	runner := sandbox.WithApproval(inner, approve, allowlist)
	result, err := sandbox.Exec(ctx, runner, "echo", []string{"hi"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec(echo hi): %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "hi\n" {
		t.Fatalf("result = %+v", result)
	}
	if approvals.Load() != 0 {
		t.Fatalf("approver called %d times for an allowlisted command", approvals.Load())
	}

	// sh -c unwrapping happens before allowlist matching.
	result, err = sandbox.Exec(ctx, runner, "sh", []string{"-c", "echo hi"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec(sh -c echo hi): %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "hi\n" {
		t.Fatalf("result = %+v", result)
	}
	if approvals.Load() != 0 {
		t.Fatalf("approver called for an unwrapped allowlisted command")
	}

	// Out-of-bounds with a nil approver fails closed without executing.
	denyRunner := sandbox.WithApproval(inner, nil, allowlist)
	if _, err := sandbox.Exec(ctx, denyRunner, "ls", nil, sandbox.ExecOptions{}); !errdefs.IsPolicyDenied(err) {
		t.Fatalf("Exec(ls) error = %v, want policy denied", err)
	}
	if approvals.Load() != 0 {
		t.Fatalf("nil approver was invoked")
	}

	// Out-of-bounds with an approving approver executes and asks once.
	runner = sandbox.WithApproval(inner, approve, allowlist)
	result, err = sandbox.Exec(ctx, runner, "ls", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec(ls): %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if approvals.Load() != 1 {
		t.Fatalf("approver calls = %d, want 1", approvals.Load())
	}
}
