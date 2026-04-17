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
	"github.com/GizClaw/flowcraft/internal/paths"
)

// Native runs `flowcraft server` as a detached child with PID + log files under ~/.flowcraft.
type Native struct{}

// NewNative constructs a Linux Native machine manager.
func NewNative() *Native {
	return &Native{}
}

var _ Machine = (*Native)(nil)

// Start launches the M1 server binary in the background.
func (n *Native) Start(ctx context.Context) error {
	_ = ctx
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	if running, _ := n.pidRunning(); running {
		return fmt.Errorf("flowcraft server already running (pid file %s)", paths.PIDFile())
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	logPath := paths.ServerLogFile()
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
	if err := os.WriteFile(paths.PIDFile(), []byte(pid+"\n"), 0o644); err != nil {
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
		return err
	}
	return nil
}

func (n *Native) pidRunning() (bool, int) {
	data, err := os.ReadFile(paths.PIDFile())
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
func (n *Native) Stop(ctx context.Context) error {
	_ = ctx
	data, err := os.ReadFile(paths.PIDFile())
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
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			_ = os.Remove(paths.PIDFile())
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(paths.PIDFile())
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL(cfg), nil)
	if err != nil {
		return st, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return st, nil
	}
	defer resp.Body.Close()
	st.HealthzOK = resp.StatusCode == http.StatusOK
	return st, nil
}

// Logs prints the server log file to w.
func (n *Native) Logs(ctx context.Context, w io.Writer) error {
	_ = ctx
	data, err := os.ReadFile(paths.ServerLogFile())
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("no server log file yet")
		}
		return err
	}
	_, err = w.Write(data)
	return err
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
