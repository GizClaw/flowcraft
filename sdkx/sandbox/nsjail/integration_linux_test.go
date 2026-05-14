//go:build integration_nsjail && linux

// These tests require a real nsjail binary on PATH and a Linux host
// that allows unprivileged user / net / cgroup namespaces (the
// default on modern Ubuntu / Debian / Fedora). They are gated behind
// the integration_nsjail build tag so the standard `go test ./...`
// lane never picks them up; CI runs them in a dedicated job that
// installs nsjail first. See .github/workflows/ci.yml ::
// test-sdkx-nsjail-integration.
//
// Tests that depend on cgroup-backed enforcement (memory / cpu caps)
// or on a specific network posture self-Skip when the host cannot
// configure that boundary, instead of failing. The intent is to
// validate "nsjail did what we asked when the kernel allowed it",
// not to gate CI on the kernel build profile of the runner.

package nsjail

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func requireNsjail(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("nsjail"); err != nil {
		t.Skipf("nsjail binary not on PATH: %v", err)
	}
}

func newIntegrationRunner(t *testing.T) *Runner {
	requireNsjail(t)
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// TestIntegration_BasicExec is the smoke test: any failure here means
// the nsjail wire-up itself is broken, not the policy layer.
func TestIntegration_BasicExec(t *testing.T) {
	r := newIntegrationRunner(t)
	res, err := r.Exec(context.Background(), "/bin/echo", []string{"hello", "world"}, sandbox.ExecOptions{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello world") {
		t.Errorf("missing stdout: got %q", res.Stdout)
	}
}

func TestIntegration_NonZeroExitPropagated(t *testing.T) {
	r := newIntegrationRunner(t)
	res, err := r.Exec(context.Background(), "/bin/sh", []string{"-c", "exit 7"}, sandbox.ExecOptions{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec returned err for non-zero exit (should be result-not-error): %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("expected exit 7, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
}

func TestIntegration_StdinForwarded(t *testing.T) {
	r := newIntegrationRunner(t)
	res, err := r.Exec(context.Background(), "/bin/cat", nil, sandbox.ExecOptions{
		Stdin:   []byte("piped-payload"),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "piped-payload") {
		t.Errorf("stdin not forwarded: stdout=%q", res.Stdout)
	}
}

func TestIntegration_EnvInject(t *testing.T) {
	r := newIntegrationRunner(t)
	res, err := r.Exec(context.Background(), "/bin/sh", []string{"-c", "echo MY_VAR=$MY_VAR"}, sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{
			Allow:  []string{"PATH"}, // /bin/sh needs nothing extra here
			Inject: map[string]string{"MY_VAR": "INJECTED-VALUE"},
		},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "MY_VAR=INJECTED-VALUE") {
		t.Errorf("env not injected: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestIntegration_EnvAllowEmptyStripsHost(t *testing.T) {
	r := newIntegrationRunner(t)
	// We probe a host-typical variable (HOME) that the test harness
	// definitely has; the child must NOT see it under Allow=[].
	t.Setenv("HOME_FROM_HOST", "should-not-leak")
	res, err := r.Exec(context.Background(), "/bin/sh", []string{"-c", "echo LEAK=${HOME_FROM_HOST:-empty}"}, sandbox.ExecOptions{
		Env:     sandbox.EnvPolicy{Allow: []string{}},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "LEAK=empty") {
		t.Errorf("host env leaked through Allow=[]: stdout=%q", res.Stdout)
	}
}

func TestIntegration_TimeoutEnforced(t *testing.T) {
	r := newIntegrationRunner(t)
	start := time.Now()
	res, err := r.Exec(context.Background(), "/bin/sleep", []string{"30"}, sandbox.ExecOptions{
		Timeout: 1 * time.Second,
	})
	elapsed := time.Since(start)

	// Wall-clock: we asked for 1s. nsjail's --time_limit is
	// whole-second resolution, and our Go-side ctx adds a SIGKILL
	// fallback. Allow generous slack for slow CI runners.
	if elapsed > 8*time.Second {
		t.Errorf("timeout not enforced: elapsed=%v", elapsed)
	}
	// Either nsjail killed the child and returned a non-zero status,
	// or the Go-side ctx fallback kicked in and we got an err. Both
	// are acceptable; what is NOT acceptable is a clean exit 0.
	if err == nil && res.ExitCode == 0 {
		t.Errorf("expected non-zero exit or err, got clean run: %+v", res)
	}
}

func TestIntegration_NetDenyAllBreaksOutbound(t *testing.T) {
	r := newIntegrationRunner(t)
	// getent hosts shells out to nsswitch (DNS / files). Inside a
	// fresh net namespace with only lo there is no DNS resolver
	// reachable, so the lookup must fail. If nsjail itself can't
	// create the namespace (rare on modern hosts but possible in
	// stripped-down kernels), surface that as a Skip — the contract
	// we're validating is "nsjail enforced what we asked when the
	// kernel cooperated", not "every CI runner can create net
	// namespaces".
	res, err := r.Exec(context.Background(), "/usr/bin/getent", []string{"hosts", "example.com"}, sandbox.ExecOptions{
		Net:     sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Skipf("NetDenyAll could not be applied by this kernel: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Errorf("expected DNS lookup to fail under NetDenyAll, got exit=0 stdout=%q", res.Stdout)
	}
}

func TestIntegration_NetDefaultDoesNotBreakBasicExec(t *testing.T) {
	r := newIntegrationRunner(t)
	// NetDefault means "inherit host net namespace"; the contract
	// is that nsjail still runs the command and the namespace
	// inheritance doesn't perturb a no-net workload.
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Net:     sandbox.NetPolicy{Mode: sandbox.NetDefault},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("NetDefault should not affect basic exec, got exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}

func TestIntegration_MemoryCapEnforced(t *testing.T) {
	r := newIntegrationRunner(t)
	// awk allocates ~100MB by concatenating into a single string.
	// MemoryBytes=16MB should trip the cgroup memory.max controller
	// and kill the process. The exit may be reported as a signal
	// (137 = 128+9) or a nsjail-side non-zero status — either is
	// fine; what we assert is "not zero".
	const memCap int64 = 16 << 20
	res, err := r.Exec(context.Background(), "/usr/bin/awk", []string{
		`BEGIN{ s=""; for(i=0;i<10000000;i++) s=s "abcdefghij"; print length(s) }`,
	}, sandbox.ExecOptions{
		Resources: sandbox.ResourceLimits{MemoryBytes: memCap},
		Timeout:   30 * time.Second,
	})
	if err != nil {
		t.Skipf("memory-cap test could not configure cgroup: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Errorf("expected OOM-kill at %d bytes, got exit=0 stdout=%q stderr=%q", memCap, res.Stdout, res.Stderr)
	}
}

func TestIntegration_WorkDirEscapeRejected(t *testing.T) {
	r := newIntegrationRunner(t)
	// /etc is unambiguously outside the t.TempDir() rootDir. The
	// rejection happens in resolveWorkDir before nsjail is even
	// invoked, so this exercises the same code path as the unit test
	// but on the same Linux box that runs the real backend.
	_, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		WorkDir: "/etc",
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatalf("expected path-traversal rejection")
	}
}
