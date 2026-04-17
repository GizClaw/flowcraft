//go:build darwin

package machine

import (
	"bytes"
	"context"
	"encoding/json"
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
	limaInstanceName = "flowcraft"
)

// LimaVM manages a FlowCraft server inside a Lima virtual machine on macOS.
type LimaVM struct {
	Version string
}

// NewLimaVM creates a macOS VM machine manager for the given runtime version.
func NewLimaVM(version string) *LimaVM {
	return &LimaVM{Version: version}
}

var _ Machine = (*LimaVM)(nil)

func (v *LimaVM) Start(ctx context.Context) error {
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	if err := requireLimactl(); err != nil {
		return err
	}

	if running, _ := v.isRunning(ctx); running {
		return errors.New("flowcraft VM is already running")
	}

	imagePath, err := EnsureImage(ctx, v.Version, ImageQCOW2)
	if err != nil {
		return fmt.Errorf("ensure VM image: %w", err)
	}

	if !v.instanceExists(ctx) {
		if err := v.createInstance(ctx, imagePath); err != nil {
			return fmt.Errorf("create lima instance: %w", err)
		}
	}

	fmt.Println("Starting FlowCraft VM...")
	if err := v.runLimactl(ctx, "start", limaInstanceName); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	fmt.Println("Waiting for healthz...")
	if err := v.waitHealthy(ctx, 60*time.Second); err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	fmt.Println("FlowCraft is ready.")
	return nil
}

func (v *LimaVM) Stop(ctx context.Context) error {
	if err := requireLimactl(); err != nil {
		return err
	}
	return v.runLimactl(ctx, "stop", limaInstanceName)
}

func (v *LimaVM) Status(ctx context.Context) (*Status, error) {
	st := &Status{}
	if err := requireLimactl(); err != nil {
		return st, nil
	}

	running, pid := v.isRunning(ctx)
	st.Running = running
	st.PID = pid
	if !running {
		return st, nil
	}

	cfg := config.Load()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL(cfg), nil)
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

func (v *LimaVM) Logs(ctx context.Context, w io.Writer) error {
	if err := requireLimactl(); err != nil {
		return err
	}
	logPath := filepath.Join(paths.MachineDir(), "vm.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("no VM log file yet")
		}
		return err
	}
	_, err = w.Write(data)
	return err
}

func (v *LimaVM) createInstance(ctx context.Context, imagePath string) error {
	cfg := config.Load()
	limaConfig := map[string]any{
		"images": []map[string]string{
			{"location": imagePath},
		},
		"cpus":   2,
		"memory": "4GiB",
		"disk":   "20GiB",
		"mounts": []map[string]any{
			{
				"location":   paths.DataDir(),
				"mountPoint": "/data",
				"writable":   true,
			},
		},
		"portForwards": []map[string]any{
			{
				"guestPort": cfg.Server.Port,
				"hostPort":  cfg.Server.Port,
			},
		},
		"provision": []map[string]string{
			{
				"mode":   "system",
				"script": "#!/bin/sh\nflowcraft server &",
			},
		},
	}

	configData, err := json.MarshalIndent(limaConfig, "", "  ")
	if err != nil {
		return err
	}
	configFile := filepath.Join(paths.MachineDir(), "lima.yaml")
	if err := os.WriteFile(configFile, configData, 0o644); err != nil {
		return err
	}

	return v.runLimactl(ctx, "create", "--name="+limaInstanceName, configFile)
}

func (v *LimaVM) instanceExists(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "limactl", "list", "--json")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), limaInstanceName)
}

func (v *LimaVM) isRunning(ctx context.Context) (bool, int) {
	cmd := exec.CommandContext(ctx, "limactl", "list", "--json")
	out, err := cmd.Output()
	if err != nil {
		return false, 0
	}

	var instances []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		PID    int    `json:"pid"`
	}
	for line := range bytes.SplitSeq(out, []byte("\n")) {
		var inst struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			PID    int    `json:"pid"`
		}
		if json.Unmarshal(line, &inst) == nil && inst.Name == limaInstanceName {
			instances = append(instances, inst)
		}
	}
	for _, inst := range instances {
		if inst.Status == "Running" {
			return true, inst.PID
		}
	}
	return false, 0
}

func (v *LimaVM) waitHealthy(ctx context.Context, timeout time.Duration) error {
	cfg := config.Load()
	url := healthURL(cfg)
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

func (v *LimaVM) runLimactl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "limactl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func requireLimactl() error {
	if _, err := exec.LookPath("limactl"); err != nil {
		return fmt.Errorf("limactl not found; install Lima: brew install lima")
	}
	return nil
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
