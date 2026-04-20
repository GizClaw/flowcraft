//go:build linux

package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	otellog "go.opentelemetry.io/otel/log"
)

// Native runs `flowcraft server` as a detached child with PID + log files under ~/.flowcraft.
type Native struct{}

// NewNative constructs a Linux Native machine manager.
func NewNative() *Native {
	return &Native{}
}

var _ Machine = (*Native)(nil)

// Start launches the M1 server binary in the background.
//
// The command is idempotent: if the server is already running and /healthz
// returns OK, Start returns nil. If the PID is alive but healthz fails (the
// process is stuck or the port is not bound), Start treats it as a stale
// instance, stops it, and relaunches. A stale PID file with no live process
// is silently cleaned up before launching.
func (n *Native) Start(ctx context.Context) error {
	if err := config.EnsureLayout(); err != nil {
		return err
	}

	cfg := config.Load()
	if running, pid := n.pidRunning(); running {
		if probeHealthz(ctx, healthURL(cfg)) {
			telemetry.Info(ctx, "cli: server already running and healthy",
				otellog.Int("pid", pid))
			return nil
		}
		telemetry.Warn(ctx, "cli: server pid alive but healthz failing — restarting",
			otellog.Int("pid", pid))
		if err := n.Stop(ctx); err != nil {
			return fmt.Errorf("stop unhealthy server: %w", err)
		}
	} else {
		_ = os.Remove(config.PIDFile())
	}

	return n.launchServer()
}

func (n *Native) launchServer() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	logPath := config.ServerLogFile()
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(exe, "server")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := os.WriteFile(config.PIDFile(), []byte(pid+"\n"), 0o644); err != nil {
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
		return err
	}
	return nil
}

// probeHealthz issues a single GET to url and returns true on HTTP 200.
// Used by Start for fast idempotency checks (not the long boot wait).
func probeHealthz(ctx context.Context, url string) bool {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (n *Native) pidRunning() (bool, int) {
	data, err := os.ReadFile(config.PIDFile())
	if err != nil {
		return false, 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false, 0
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false, pid
	}
	return true, pid
}

// Stop terminates the background server using the PID file.
//
// Sends SIGTERM and waits up to 10s for graceful exit, then SIGKILL.
// Each phase is logged so users can see why a stop is taking time.
func (n *Native) Stop(ctx context.Context) error {
	data, err := os.ReadFile(config.PIDFile())
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("flowcraft server is not running (no pid file)")
		}
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("invalid pid file")
	}

	if err := syscall.Kill(pid, 0); err != nil {
		telemetry.Info(ctx, "cli: server already stopped — clearing stale pid file",
			otellog.Int("pid", pid))
		_ = os.Remove(config.PIDFile())
		return nil
	}

	telemetry.Info(ctx, "cli: stopping server (SIGTERM)", otellog.Int("pid", pid))
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			_ = os.Remove(config.PIDFile())
			telemetry.Info(ctx, "cli: server stopped", otellog.Int("pid", pid))
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	telemetry.Warn(ctx, "cli: server did not exit within 10s — sending SIGKILL",
		otellog.Int("pid", pid))
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(config.PIDFile())
	telemetry.Info(ctx, "cli: server killed", otellog.Int("pid", pid))
	return nil
}

// Status reports whether the PID is alive and /healthz responds.
func (n *Native) Status(ctx context.Context) (*Status, error) {
	running, pid := n.pidRunning()
	st := &Status{Running: running, PID: pid}
	if !running {
		return st, nil
	}
	cfg := config.Load()
	st.HealthzOK = probeHealthz(ctx, healthURL(cfg))
	return st, nil
}

// Logs prints the server log file to w.
func (n *Native) Logs(ctx context.Context, w io.Writer) error {
	_ = ctx
	data, err := os.ReadFile(config.ServerLogFile())
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("no server log file yet")
		}
		return err
	}
	_, err = w.Write(data)
	return err
}

func (n *Native) Reset(ctx context.Context, scope ResetScope) error {
	_ = n.Stop(ctx)
	switch scope {
	case ResetMachine:
		_ = os.RemoveAll(config.MachineDir())
		_ = os.Remove(config.PIDFile())
		return nil
	case ResetData:
		return os.RemoveAll(config.DataDir())
	default:
		return os.RemoveAll(config.HomeRoot())
	}
}

func (n *Native) OpenWeb(ctx context.Context) error {
	if running, _ := n.pidRunning(); !running {
		return errors.New("server is not running")
	}
	cfg := config.Load()
	return exec.CommandContext(ctx, "xdg-open", baseURL(cfg)).Run()
}

func baseURL(cfg *config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Address())
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "[::]" {
		host = "::1"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(host, port))
}

func healthURL(cfg *config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Address())
	if err != nil {
		return "http://127.0.0.1:8080/healthz"
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "[::]" {
		host = "::1"
	}
	return fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))
}
