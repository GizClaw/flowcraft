//go:build integration_bwrap && linux

// These tests require a real bwrap binary on PATH and a Linux host
// that allows unprivileged user / mount / net namespaces (the
// default on modern Ubuntu / Debian / Fedora and WSL2). They are gated
// behind the integration_bwrap build tag so the standard `go test
// ./...` lane never picks them up; CI runs them in a dedicated job
// that installs bubblewrap first. Hosts that can install the binary
// but cannot build bwrap's mount tree skip the lane rather than fail.
// See .github/workflows/ci.yml :: test-sdkx-bwrap-integration.
//
// Tests that depend on the shared process-group watcher (memory /
// cpu caps) or on a specific network posture self-Skip when the host
// cannot configure that boundary, instead of failing. The intent is to
// validate "bwrap did what we asked when the kernel allowed it",
// not to gate CI on the kernel build profile of the runner.

package bwrap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap binary not on PATH: %v", err)
	}
	if err := probeBwrap(); err != nil {
		t.Skipf("bwrap binary present but unusable on this host: %v", err)
	}
}

var (
	bwrapProbeOnce sync.Once
	bwrapProbeErr  error
)

// probeBwrap runs one minimal exec to verify the host can build
// bwrap's mount tree. Hosts that cannot create the required mount /
// net namespaces skip the integration lane instead of failing, while
// capable hosts still exercise the real backend.
func probeBwrap() error {
	bwrapProbeOnce.Do(func() {
		root, err := os.MkdirTemp("", "flowcraft-bwrap-probe-")
		if err != nil {
			bwrapProbeErr = err
			return
		}
		defer os.RemoveAll(root)
		if err := os.Chmod(root, 0o755); err != nil {
			bwrapProbeErr = err
			return
		}
		runner, err := New(root)
		if err != nil {
			bwrapProbeErr = err
			return
		}
		res, err := runner.Exec(
			context.Background(),
			"/bin/true",
			nil,
			sandbox.ExecOptions{Timeout: 5 * time.Second},
		)
		if err != nil {
			bwrapProbeErr = err
			return
		}
		if res.ExitCode != 0 {
			bwrapProbeErr = fmt.Errorf(
				"probe exit=%d stderr=%q",
				res.ExitCode,
				res.Stderr,
			)
		}
	})
	return bwrapProbeErr
}

// newIntegrationRoot returns a sandbox root that is traversable by the
// bwrap child. t.TempDir creates 0700 directories, which trips the
// bind-mount step when the runner is invoked through a different uid
// mapping; 0755 keeps the source path walkable in every configuration.
func newIntegrationRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod sandbox root: %v", err)
	}
	return root
}

func newIntegrationRunner(t *testing.T) *Runner {
	requireBwrap(t)
	r, err := New(newIntegrationRoot(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// newIntegrationProxyRunner builds a runner for NetAllowList /
// NetProxy integration cases. The in-netns bridge is the test binary
// itself: the runner re-executes os.Executable() (this test binary)
// with bridge.Marker, and TestMain in dispatch_test.go hands control
// to the bridge implementation. No separate bridge binary is built.
func newIntegrationProxyRunner(t *testing.T) *Runner {
	t.Helper()
	requireBwrap(t)
	r, err := New(newIntegrationRoot(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not on PATH: %v", err)
	}
}

// TestIntegration_BasicExec is the smoke test: any failure here means
// the bwrap wire-up itself is broken, not the policy layer.
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

	// Wall-clock: we asked for 1s. The Go-side ctx kills the whole
	// process group (and --die-with-parent kills the tree). Allow
	// generous slack for slow CI runners.
	if elapsed > 8*time.Second {
		t.Errorf("timeout not enforced: elapsed=%v", elapsed)
	}
	// Either bwrap reported a non-zero status, or the Go-side ctx
	// fallback kicked in and we got an err. Both are acceptable; what
	// is NOT acceptable is a clean exit 0.
	if err == nil && res.ExitCode == 0 {
		t.Errorf("expected non-zero exit or err, got clean run: %+v", res)
	}
}

func TestIntegration_NetDenyAllBreaksOutbound(t *testing.T) {
	r := newIntegrationRunner(t)
	// getent hosts shells out to nsswitch (DNS / files). Inside a
	// fresh net namespace with only lo there is no DNS resolver
	// reachable, so the lookup must fail. If bwrap itself can't
	// create the namespace (rare on modern hosts but possible in
	// stripped-down kernels), surface that as a Skip — the contract
	// we're validating is "bwrap enforced what we asked when the
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
	// is that bwrap still runs the command and the namespace
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
	// The probe holds ~64MB of process RSS (command substitution of a
	// 64MiB stream) for a few seconds. MemoryBytes=16MB should trip the
	// shared group watcher, which SIGKILLs the whole group. The cap may
	// surface as a BudgetExceeded error (watcher fired) or a non-zero
	// exit (process died first) — either is fine; what we assert is
	// "not a clean exit 0".
	const memCap int64 = 16 << 20
	res, err := r.Exec(context.Background(), "/bin/sh", []string{
		"-c", `x=$(head -c 67108864 /dev/zero | tr "\000" x); echo allocated ${#x}; sleep 3`,
	}, sandbox.ExecOptions{
		Resources: sandbox.ResourceLimits{MemoryBytes: memCap},
		Timeout:   30 * time.Second,
	})
	if err != nil {
		if errdefs.IsBudgetExceeded(err) {
			return // cap enforced by the watcher
		}
		t.Skipf("memory-cap test could not run the group watcher: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Errorf("expected OOM-kill at %d bytes, got exit=0 stdout=%q stderr=%q", memCap, res.Stdout, res.Stderr)
	}
}

func TestIntegration_WorkDirEscapeRejected(t *testing.T) {
	r := newIntegrationRunner(t)
	// /etc is unambiguously outside the t.TempDir() rootDir. The
	// rejection happens in resolveWorkDir before bwrap is even
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

func TestIntegration_FilesystemBounds(t *testing.T) {
	requireBwrap(t)
	root := newIntegrationRoot(t)
	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(
		home,
		fmt.Sprintf(".flowcraft-bwrap-outside-%d", os.Getpid()),
	)
	_ = os.Remove(outside)
	t.Cleanup(func() { _ = os.Remove(outside) })

	inside := filepath.Join(root, "inside.txt")
	result, err := runner.Exec(
		context.Background(),
		"/bin/sh",
		[]string{"-c", `printf inside > "$1"; printf outside > "$2"`, "sh", inside, outside},
		sandbox.ExecOptions{Timeout: 5 * time.Second},
	)
	if err != nil {
		stderr := ""
		if result != nil {
			stderr = result.Stderr
		}
		t.Fatalf("Exec: %v (stderr=%q)", err, stderr)
	}
	if result.ExitCode == 0 {
		t.Fatalf("write outside workspace unexpectedly succeeded: %+v", result)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "inside" {
		t.Fatalf("workspace write failed: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside file should not exist on host, stat err=%v", err)
	}
}

func TestIntegration_PrivateTmpDoesNotReachHost(t *testing.T) {
	runner := newIntegrationRunner(t)
	marker := filepath.Join(
		"/tmp",
		fmt.Sprintf("flowcraft-bwrap-private-%d", os.Getpid()),
	)
	_ = os.Remove(marker)
	t.Cleanup(func() { _ = os.Remove(marker) })

	result, err := runner.Exec(
		context.Background(),
		"/bin/sh",
		[]string{"-c", `printf private > "$1"`, "sh", marker},
		sandbox.ExecOptions{Timeout: 5 * time.Second},
	)
	if err != nil {
		stderr := ""
		if result != nil {
			stderr = result.Stderr
		}
		t.Fatalf("Exec: %v (stderr=%q)", err, stderr)
	}
	if result.ExitCode != 0 {
		t.Fatalf("private /tmp should be writable: %+v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("private /tmp write leaked to host, stat err=%v", err)
	}
}

func TestIntegration_SymlinkCannotEscapeWritableRoot(t *testing.T) {
	requireBwrap(t)
	root := newIntegrationRoot(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(
		home,
		fmt.Sprintf(".flowcraft-bwrap-symlink-%d", os.Getpid()),
	)
	_ = os.Remove(outside)
	t.Cleanup(func() { _ = os.Remove(outside) })

	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/bin/sh",
		[]string{"-c", `printf escaped > "$1"`, "sh", filepath.Join(link, filepath.Base(outside))},
		sandbox.ExecOptions{Timeout: 5 * time.Second},
	)
	if err != nil {
		stderr := ""
		if result != nil {
			stderr = result.Stderr
		}
		t.Fatalf("Exec: %v (stderr=%q)", err, stderr)
	}
	if result.ExitCode == 0 {
		t.Fatalf("symlink escape unexpectedly succeeded: %+v", result)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("symlink escape created host file, stat err=%v", err)
	}
}

func TestIntegration_NetAllowListAllowsListedHost(t *testing.T) {
	requireCurl(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "allowed-ok")
	}))
	defer srv.Close()

	runner := newIntegrationProxyRunner(t)
	res, err := runner.Exec(context.Background(), "/usr/bin/curl", []string{"-s", srv.URL}, sandbox.ExecOptions{
		Net:     sandbox.NetPolicy{Mode: sandbox.NetAllowList, AllowHosts: []string{"127.0.0.1"}},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	if !strings.Contains(res.Stdout, "allowed-ok") {
		t.Errorf("response not forwarded through the proxy: stdout=%q", res.Stdout)
	}
}

func TestIntegration_NetAllowListDeniesUnlisted(t *testing.T) {
	requireCurl(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should-not-arrive")
	}))
	defer srv.Close()

	runner := newIntegrationProxyRunner(t)
	// -f turns the proxy's HTTP 403 into a non-zero curl exit; a 403
	// response body alone would otherwise leave curl at exit 0.
	res, err := runner.Exec(context.Background(), "/usr/bin/curl", []string{"-s", "-f", srv.URL}, sandbox.ExecOptions{
		Net:     sandbox.NetPolicy{Mode: sandbox.NetAllowList, AllowHosts: []string{"example.com"}},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Errorf("unlisted destination must fail, got exit=0 stdout=%q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "should-not-arrive") {
		t.Errorf("denied destination received the response body: %q", res.Stdout)
	}
}

func TestIntegration_NetProxyForwardsToUpstream(t *testing.T) {
	requireCurl(t)
	var (
		mu  sync.Mutex
		hit bool
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit = true
		mu.Unlock()
		fmt.Fprint(w, "upstream-ok")
	}))
	defer upstream.Close()

	runner := newIntegrationProxyRunner(t)
	// Port 1 is never reachable directly; success proves the request
	// was routed through the upstream proxy.
	res, err := runner.Exec(context.Background(), "/usr/bin/curl", []string{"-s", "http://127.0.0.1:1/hello"}, sandbox.ExecOptions{
		Net:     sandbox.NetPolicy{Mode: sandbox.NetProxy, Proxy: upstream.URL},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	if !strings.Contains(res.Stdout, "upstream-ok") {
		t.Errorf("upstream response not delivered: stdout=%q", res.Stdout)
	}
	mu.Lock()
	defer mu.Unlock()
	if !hit {
		t.Errorf("upstream proxy never received the request")
	}
}

func TestIntegration_IsolatedModesMaskHostUDS(t *testing.T) {
	runner := newIntegrationRunner(t)
	// The isolated net modes mount a private tmpfs at /run: writing a
	// marker there must succeed (host /run is not user-writable), host
	// unix sockets must not be visible, and the marker must not leak to
	// the host.
	marker := "/run/flowcraft-bwrap-marker"
	_ = os.Remove(marker)
	t.Cleanup(func() { _ = os.Remove(marker) })

	res, err := runner.Exec(context.Background(), "/bin/sh", []string{"-c",
		"echo masked > " + marker + "; test ! -S /run/docker.sock; cat " + marker}, sandbox.ExecOptions{
		Net:     sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	if !strings.Contains(res.Stdout, "masked") {
		t.Errorf("expected writable private /run, stdout=%q", res.Stdout)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("private /run marker leaked to host, stat err=%v", err)
	}
}
