//go:build integration_windows

package windows

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
	"golang.org/x/sys/windows"
)

// TestMain gives the re-executed test binary a host-main hook so the
// elevated helper (which re-executes the current executable with
// HelperArgvMarker) can serve under `go test`. Real host applications
// call windows.MaybeHelper() from their own main; the testing binary
// has no main of ours, so intercept the marker before m.Run parses
// flags.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArgvMarker {
		os.Exit(runHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// This file is the runtime verification layer for the Windows
// backend: it exercises the real Win32 primitives (restricted token,
// ConPTY, job object, and — when the process is elevated — account
// creation, WFP filters, and the elevated helper pipe). Run with
// `-tags=integration_windows` on a Windows machine; the elevated
// tests skip themselves unless the process token is elevated.
//
// NOTE: the elevated tests mutate the machine: they create two local
// accounts (FlowCraftSbxOffline / FlowCraftSbxOnline) and
// install persistent WFP filters. Run them only on a throwaway CI
// runner or a machine you can clean up.

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestIntegrationExecEcho(t *testing.T) {
	r := newTestRunner(t)
	res, err := sandbox.Exec(
		context.Background(), r,
		"cmd", []string{"/c", "echo", "hello"},
		sandbox.ExecOptions{},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("result = %+v", res)
	}
}

func TestIntegrationExecEnvAllowlist(t *testing.T) {
	r := newTestRunner(t)
	res, err := sandbox.Exec(
		context.Background(), r,
		"cmd", []string{"/c", "echo", "%FOO%"},
		sandbox.ExecOptions{
			Env: sandbox.EnvPolicy{Allow: []string{}, Inject: map[string]string{"FOO": "bar"}},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "bar") {
		t.Fatalf("stdout = %q, want injected FOO=bar", res.Stdout)
	}
}

func TestIntegrationExecWorkDirTraversal(t *testing.T) {
	r := newTestRunner(t)
	_, err := sandbox.Exec(
		context.Background(), r,
		"cmd", nil,
		sandbox.ExecOptions{WorkDir: ".."},
	)
	if !errors.Is(err, sandbox.ErrPathTraversal) {
		t.Fatalf("Exec(..) error = %v, want ErrPathTraversal", err)
	}
}

func TestIntegrationExecTimeout(t *testing.T) {
	r := newTestRunner(t)
	_, err := sandbox.Exec(
		context.Background(), r,
		"cmd", []string{"/c", "ping", "-n", "5", "127.0.0.1"},
		sandbox.ExecOptions{Timeout: 1 * time.Second},
	)
	if !errdefs.IsTimeout(err) {
		t.Fatalf("Exec timeout error = %v, want timeout", err)
	}
}

func TestIntegrationExecCPUCap(t *testing.T) {
	r := newTestRunner(t)
	_, err := sandbox.Exec(
		context.Background(), r,
		"powershell", []string{"-NoProfile", "-Command", "while($true){}"},
		sandbox.ExecOptions{
			Timeout: 10 * time.Second,
			Resources: sandbox.ResourceLimits{
				CPUMillicores: 100, // 10s x 0.1 = 1s cpu-time budget
			},
		},
	)
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("Exec cpu cap error = %v, want BudgetExceeded", err)
	}
}

func TestIntegrationExecMemoryCap(t *testing.T) {
	r := newTestRunner(t)
	_, err := sandbox.Exec(
		context.Background(), r,
		"powershell", []string{
			"-NoProfile", "-Command",
			"while($true){ $a = New-Object byte[] 134217728 }",
		},
		sandbox.ExecOptions{
			Timeout:   20 * time.Second,
			Resources: sandbox.ResourceLimits{MemoryBytes: 64 * 1024 * 1024},
		},
	)
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("Exec memory cap error = %v, want BudgetExceeded", err)
	}
}

func TestIntegrationTTYSession(t *testing.T) {
	r := newTestRunner(t)
	sess, err := r.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"cmd", "/c", "echo hello"},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out strings.Builder
	after := int64(0)
	for {
		o, err := sess.Read(ctx, after, 64*1024)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, ch := range o.Chunks {
			out.Write(ch.Data)
		}
		after = o.NextSeq
		if strings.Contains(out.String(), "hello") {
			break
		}
		if o.EOF {
			t.Fatalf("session EOF before marker; output = %q", out.String())
		}
	}
	exit, err := sess.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Reason != sandbox.SessionExited {
		t.Fatalf("exit = %+v, want exited", exit)
	}
}

func requireElevated(t *testing.T) {
	t.Helper()
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		t.Skipf("cannot open process token: %v", err)
	}
	defer func() { _ = tok.Close() }()
	if !tok.IsElevated() {
		t.Skip("process is not elevated; skipping elevated backend integration")
	}
}

func TestIntegrationElevatedSpawn(t *testing.T) {
	requireElevated(t)
	r, err := New(t.TempDir(), WithLevel(LevelElevated))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	// NetDefault runs children as the online account.
	res, err := sandbox.Exec(
		context.Background(), r,
		"cmd", []string{"/c", "whoami"},
		sandbox.ExecOptions{},
	)
	if err != nil {
		t.Fatalf("Exec(elevated): %v", err)
	}
	if !strings.Contains(strings.ToLower(res.Stdout), "flowcraftsbxonline") {
		t.Fatalf("whoami = %q, want online sandbox account", res.Stdout)
	}

	// NetDenyAll runs children as the offline account and blocks
	// outbound traffic via the WFP filters installed during setup.
	res, err = sandbox.Exec(
		context.Background(), r,
		"cmd", []string{"/c", "whoami"},
		sandbox.ExecOptions{Net: corenet.NetPolicy{Mode: corenet.NetDenyAll}},
	)
	if err != nil {
		t.Fatalf("Exec(elevated, denyall): %v", err)
	}
	if !strings.Contains(strings.ToLower(res.Stdout), "flowcraftsbxoffline") {
		t.Fatalf("whoami = %q, want offline sandbox account", res.Stdout)
	}

	// The offline account must not be able to reach the internet.
	blocked, err := sandbox.Exec(
		context.Background(), r,
		"cmd", []string{"/c", "curl.exe", "-s", "--max-time", "8", "-o", "NUL", "https://1.1.1.1"},
		sandbox.ExecOptions{Net: corenet.NetPolicy{Mode: corenet.NetDenyAll}},
	)
	if err != nil {
		t.Fatalf("Exec(curl, denyall): %v", err)
	}
	if blocked.ExitCode == 0 {
		t.Fatal("offline sandbox account reached the internet; WFP block did not apply")
	}
}

// TestIntegrationLogonUserW isolates the LogonUserW primitive from
// the helper: same account, same password path, but running directly
// in the test process (also elevated). If this passes while the
// helper dies at the same call, the fault is in the helper's state
// (e.g. memory corruption during setup); if it also dies, the CI
// environment's LogonUserW is at fault.
func TestIntegrationLogonUserW(t *testing.T) {
	requireElevated(t)
	dir := t.TempDir()
	if err := SandboxHelperInstall(dir); err != nil {
		t.Fatalf("install: %v", err)
	}
	creds, err := loadCreds(dir)
	if err != nil {
		t.Fatalf("loadCreds: %v", err)
	}
	tok, err := logonSandboxUser(creds.Online, dir)
	if err != nil {
		t.Fatalf("logonSandboxUser: %v", err)
	}
	defer func() { _ = tok.Close() }()
	if tok == 0 {
		t.Fatal("logon returned zero token")
	}
}
