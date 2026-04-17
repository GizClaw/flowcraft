//go:build windows

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
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/paths"
)

const (
	wslDistroName = "flowcraft-linux"
)

// WSL manages a FlowCraft server inside a WSL2 distribution on Windows.
type WSL struct {
	Version string
}

// NewWSL creates a Windows WSL machine manager for the given runtime version.
func NewWSL(version string) *WSL {
	return &WSL{Version: version}
}

var _ Machine = (*WSL)(nil)

func (w *WSL) Start(ctx context.Context) error {
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	if err := requireWSL(); err != nil {
		return err
	}

	if w.distroExists() {
		if w.isRunning() {
			return errors.New("flowcraft WSL distro is already running")
		}
	} else {
		rootfs, err := EnsureImage(ctx, w.Version, ImageRootFS)
		if err != nil {
			return fmt.Errorf("ensure rootfs: %w", err)
		}
		installDir := filepath.Join(paths.MachineDir(), wslDistroName)
		if err := os.MkdirAll(installDir, 0o755); err != nil {
			return err
		}
		fmt.Printf("Importing WSL distro %s...\n", wslDistroName)
		cmd := exec.CommandContext(ctx, "wsl", "--import", wslDistroName, installDir, rootfs, "--version", "2")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("wsl --import: %w", err)
		}
	}

	fmt.Println("Starting FlowCraft in WSL...")
	cmd := exec.CommandContext(ctx, "wsl", "-d", wslDistroName, "--", "flowcraft", "server")
	logPath := filepath.Join(paths.MachineDir(), "wsl.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("start WSL: %w", err)
	}
	logf.Close()

	fmt.Println("Waiting for healthz...")
	if err := w.waitHealthy(ctx, 60*time.Second); err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	fmt.Println("FlowCraft is ready.")
	return nil
}

func (w *WSL) Stop(ctx context.Context) error {
	if err := requireWSL(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "wsl", "-t", wslDistroName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (w *WSL) Status(ctx context.Context) (*Status, error) {
	st := &Status{}
	if err := requireWSL(); err != nil {
		return st, nil
	}
	st.Running = w.isRunning()

	if !st.Running {
		return st, nil
	}

	cfg := config.Load()
	url := wslHealthURL(cfg)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return st, nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return st, nil
	}
	defer resp.Body.Close()
	st.HealthzOK = resp.StatusCode == http.StatusOK
	return st, nil
}

func (w *WSL) Logs(ctx context.Context, writer io.Writer) error {
	logPath := filepath.Join(paths.MachineDir(), "wsl.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("no WSL log file yet")
		}
		return err
	}
	_, err = writer.Write(data)
	return err
}

func (w *WSL) distroExists() bool {
	out, err := exec.Command("wsl", "-l", "-q").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == wslDistroName {
			return true
		}
	}
	return false
}

func (w *WSL) isRunning() bool {
	out, err := exec.Command("wsl", "-l", "--running", "-q").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == wslDistroName {
			return true
		}
	}
	return false
}

func (w *WSL) waitHealthy(ctx context.Context, timeout time.Duration) error {
	cfg := config.Load()
	url := wslHealthURL(cfg)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("timeout waiting for healthz")
}

func requireWSL() error {
	if _, err := exec.LookPath("wsl"); err != nil {
		return fmt.Errorf("WSL not available; please enable WSL2: https://aka.ms/wsl2-install")
	}
	out, err := exec.Command("wsl", "--status").CombinedOutput()
	if err != nil {
		return fmt.Errorf("WSL check failed: %s\nPlease install WSL2: https://aka.ms/wsl2-install", string(out))
	}
	return nil
}

func wslHealthURL(cfg *config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Address())
	if err != nil {
		return "http://127.0.0.1:8080/healthz"
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))
}
