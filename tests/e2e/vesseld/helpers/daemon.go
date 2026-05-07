package helpers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// DaemonHandle is the test-side view of a running vesseld
// subprocess. It owns the temp socket / config files and surfaces
// the methods e2e tests need: a real http.Client wired to the
// unix socket, a Stop helper that sends SIGTERM and waits, and the
// captured stderr buffer for assertion / debugging.
type DaemonHandle struct {
	BinaryPath string
	SocketPath string
	ConfigDir  string

	cmd       *exec.Cmd
	stderr    *syncBuffer
	exited    chan struct{}
	exitErr   error
	stopOnce  sync.Once
	stopErr   error
	bin       string
	tearDowns []func()
}

// sharedBinary is built once per `go test` invocation by
// EnsureBinary and reused across every e2e test. The first caller
// pays the ~1s build+link cost; subsequent callers return
// instantly. Rebuilding per-test was the original design but
// `go build -o <unique-path>` makes the linker re-link every time
// (build cache covers compile, not link) so the cost grew with
// the test count for no isolation benefit — Go's build cache
// already guarantees content-addressed correctness.
var (
	sharedBinaryOnce sync.Once
	sharedBinary     string
	sharedBinaryErr  error
)

// EnsureBinary returns a path to the vesseld binary, building it
// the first time it is called. Concurrent callers race on
// sync.Once and observe the same binary path. The binary is
// written into testing.M's TempDir-equivalent (os.MkdirTemp with
// `vesseld-e2e-bin-`) and removed when the test process exits.
//
// Tests SHOULD invoke this from TestMain to fail fast if the
// build itself is broken; calling from a single test works too
// but the failure surfaces only when that test runs.
func EnsureBinary(t *testing.T) string {
	t.Helper()
	if path, err := ensureBinary(); err != nil {
		t.Fatalf("e2e: build vesseld: %v", err)
		return ""
	} else {
		return path
	}
}

// ShortTempDir returns a tempdir whose path is short enough to
// host a unix socket under macOS' 104-byte sun_path cap. Tests
// that hand-roll daemon launches (instead of going through
// StartDaemon) call this to dodge the cap; cleanup is registered
// via t.Cleanup.
func ShortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "vd-")
	if err != nil {
		t.Fatalf("e2e: short tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// EnsureBinaryNoT is the TestMain-friendly variant: takes no
// *testing.T because TestMain receives only *testing.M and we
// want the build break to surface as a clear log + non-zero
// exit code rather than t.Fatal.
func EnsureBinaryNoT() (string, error) { return ensureBinary() }

func ensureBinary() (string, error) {
	sharedBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "vesseld-e2e-bin-")
		if err != nil {
			sharedBinaryErr = fmt.Errorf("mktempdir: %w", err)
			return
		}
		out := filepath.Join(dir, "vesseld")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", out, "github.com/GizClaw/flowcraft/cmd/vesseld")
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		if err := cmd.Run(); err != nil {
			_ = os.RemoveAll(dir)
			sharedBinaryErr = fmt.Errorf("go build: %w", err)
			return
		}
		sharedBinary = out
	})
	return sharedBinary, sharedBinaryErr
}

// StartDaemon writes the supplied YAML config under a per-test
// temp dir, picks a unix-socket path inside the same dir, and
// exec's the daemon. It blocks until /healthz returns 200 (or
// startupTimeout expires) so callers can issue requests
// immediately on return.
//
// The cleanup is registered with t.Cleanup so even t.Fatal mid-
// test still terminates the subprocess. Forgetting to defer Stop
// is therefore non-fatal — but tests SHOULD call Stop explicitly
// when they want to assert clean shutdown semantics.
func StartDaemon(t *testing.T, binary, configYAML string) *DaemonHandle {
	t.Helper()
	dir := t.TempDir()
	// macOS limits unix socket paths to 104 bytes (sun_path);
	// t.TempDir() roots paths under /var/folders/.../<test-name>/
	// which routinely overflows for descriptive Go test names.
	// Fall back to a short os.MkdirTemp dir whose name is just a
	// random suffix so the socket path stays well under the cap.
	socket := filepath.Join(dir, "v.sock")
	if len(socket) > 100 {
		short, err := os.MkdirTemp("", "vd-")
		if err != nil {
			t.Fatalf("e2e: short tempdir: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(short) })
		socket = filepath.Join(short, "v.sock")
		dir = short
	}

	cfg := strings.ReplaceAll(configYAML, "__SOCKET__", socket)
	cfgPath := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("e2e: write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "run", "--config", cfgPath)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	cmd.Stdout = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("e2e: start vesseld: %v", err)
	}

	h := &DaemonHandle{
		BinaryPath: binary,
		SocketPath: socket,
		ConfigDir:  dir,
		cmd:        cmd,
		stderr:     stderr,
		exited:     make(chan struct{}),
		bin:        binary,
		tearDowns:  []func(){cancel},
	}
	go func() {
		h.exitErr = cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() { _ = h.Stop(5 * time.Second) })

	if err := waitForSocket(socket, 5*time.Second); err != nil {
		t.Fatalf("e2e: vesseld did not bind socket: %v\nstderr:\n%s", err, h.Stderr())
	}
	if err := waitForHealthz(socket, 5*time.Second); err != nil {
		t.Fatalf("e2e: vesseld /healthz never returned 200: %v\nstderr:\n%s", err, h.Stderr())
	}
	return h
}

// Stop sends SIGTERM and waits up to budget for the process to
// exit. Returns the wrapped exit error from the underlying Wait
// call. Idempotent: calling Stop after the daemon has already
// exited is a no-op.
func (h *DaemonHandle) Stop(budget time.Duration) error {
	h.stopOnce.Do(func() {
		select {
		case <-h.exited:
			h.stopErr = h.exitErr
			return
		default:
		}
		_ = h.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-h.exited:
			h.stopErr = h.exitErr
		case <-time.After(budget):
			_ = h.cmd.Process.Kill()
			<-h.exited
			h.stopErr = fmt.Errorf("e2e: vesseld did not exit within %s; killed", budget)
		}
		for _, fn := range h.tearDowns {
			fn()
		}
	})
	return h.stopErr
}

// Stderr returns the captured stderr+stdout text. Useful in test
// failure messages so the user sees the daemon log without having
// to re-run.
func (h *DaemonHandle) Stderr() string { return h.stderr.String() }

// HTTPClient returns an http.Client whose transport dials the
// daemon's unix socket. The base URL for any request is
// `http://vesseld` — the host name is irrelevant because the
// transport ignores it.
func (h *DaemonHandle) HTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", h.SocketPath)
			},
		},
		Timeout: 10 * time.Second,
	}
}

// waitForSocket polls until the unix socket path exists OR budget
// expires. We watch the file rather than dialing because the
// daemon may bind the path before the listener is fully ready,
// and waitForHealthz handles the second half.
func waitForSocket(path string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not appear within %s", path, budget)
}

// waitForHealthz polls /healthz until 200 OR budget expires.
func waitForHealthz(socket string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	defer tr.CloseIdleConnections()
	cli := &http.Client{Transport: tr, Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := cli.Get("http://vesseld/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("/healthz never returned 200 within %s", budget)
}

// syncBuffer is a goroutine-safe bytes.Buffer-equivalent. exec.Cmd
// wires stderr from the child's goroutine while tests read from
// the test goroutine, so a plain bytes.Buffer would race.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
