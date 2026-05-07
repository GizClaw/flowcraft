//go:build e2e

package vesseld_e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_OS_StaleSocketCleanup asserts the daemon cleans up a
// pre-existing socket file at startup. Without this behaviour a
// previous-run crash that left the socket on disk would block the
// next start with a confusing "address already in use".
func TestE2E_OS_StaleSocketCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)

	dir := helpers.ShortTempDir(t)
	socket := filepath.Join(dir, "v.sock")
	// Plant a stale file at the socket path. Bind/unbind any old
	// fd would also work; a plain regular file is the worst case
	// because Bind on AF_UNIX rejects existing-of-any-type unless
	// the daemon explicitly removes first.
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatalf("plant stale: %v", err)
	}

	cfg := strings.ReplaceAll(configTemplate, "__OPENAI_URL__", mock.URL())
	cfg = strings.ReplaceAll(cfg, "__SOCKET__", socket)
	cfgPath := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "run", "--config", cfgPath)
	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	}()

	// Within 5s the daemon should have cleaned up the stale file
	// and bound a fresh socket.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon did not bind socket within 5s; output:\n%s", out.String())
}

// TestE2E_OS_SIGKILL_LeavesNoStaleListener asserts that even after
// SIGKILL (no graceful shutdown), the OS reclaims the socket and
// a fresh daemon can start at the same path. This is a stronger
// version of StaleSocketCleanup — SIGKILL is the worst-case for
// the OS releasing inode handles.
func TestE2E_OS_SIGKILL_LeavesNoStaleListener(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)

	d1 := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))
	socket := d1.SocketPath
	if err := syscall.Kill(d1.Pid(), syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	if err := d1.WaitExit(5 * time.Second); err == nil {
		// Wait error after SIGKILL is expected; a clean nil is
		// the surprise — our process killed itself with SIGKILL?
		// Either way, the listener-reclaim assertion below stands.
		t.Logf("first daemon exited cleanly after SIGKILL — unexpected but harmless")
	}

	// Start a second daemon at the SAME socket path. We bypass
	// helpers.StartDaemon (which picks its own random path) and
	// hand-roll the launch with the planted socket location.
	dir := helpers.ShortTempDir(t)
	cfg := strings.ReplaceAll(configTemplate, "__OPENAI_URL__", mock.URL())
	cfg = strings.ReplaceAll(cfg, "__SOCKET__", socket)
	cfgPath := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "run", "--config", cfgPath)
	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start second: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", socket)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("second daemon could not reclaim socket %s; output:\n%s", socket, out.String())
}

// TestE2E_OS_MultipleListeners asserts a daemon can simultaneously
// bind both unix and TCP listeners and answer on each. We re-use
// the auth fixture (which configures both) but talk to the unix
// path AND the TCP listener in parallel.
func TestE2E_OS_MultipleListeners(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()

	dir := t.TempDir()
	tokenPath := writeTokenFile(t, dir, "tok-mc")
	listen := pickFreePort(t)
	cfg := authTemplate
	cfg = strings.ReplaceAll(cfg, "__OPENAI_URL__", mock.URL())
	cfg = strings.ReplaceAll(cfg, "__LISTEN__", listen)
	cfg = strings.ReplaceAll(cfg, "__TOKEN_FILE__", tokenPath)

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, cfg)

	// unix /healthz
	d.MustHTTP(t, http.MethodGet, "/healthz", http.StatusOK)

	// tcp /healthz with token
	tcp := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/healthz", listen), nil)
	req.Header.Set("Authorization", "Bearer tok-mc")
	resp, err := tcp.Do(req)
	if err != nil {
		t.Fatalf("tcp healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tcp healthz status=%d", resp.StatusCode)
	}
}
