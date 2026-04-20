// Package darwin implements the flowcraft Machine lifecycle for macOS using
// vfkit (Apple Virtualization.framework) to run the server inside a Debian
// Linux VM with Bubblewrap sandbox support.
package darwin

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
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	otellog "go.opentelemetry.io/otel/log"
)

// ImageResolver downloads or locates a runtime image, returning its local path.
type ImageResolver func(ctx context.Context, version string) (string, error)

// Status describes the VM state.
type Status struct {
	Running   bool
	PID       int
	HealthzOK bool
}

// VM manages a FlowCraft server inside a vfkit virtual machine on macOS.
type VM struct {
	Version        string
	DiskResolver   ImageResolver
	BinaryResolver ImageResolver
}

// NewVM creates a macOS VM machine manager.
func NewVM(version string, diskResolver, binaryResolver ImageResolver) *VM {
	return &VM{
		Version:        version,
		DiskResolver:   diskResolver,
		BinaryResolver: binaryResolver,
	}
}

// IsProvisioned checks whether the VM has been provisioned by looking for
// the marker file that cloud-init writes to the shared data directory.
func IsProvisioned() bool {
	_, err := os.Stat(filepath.Join(config.DataDir(), ".provisioned"))
	return err == nil
}

func (v *VM) Start(ctx context.Context) error {
	if err := config.EnsureLayout(); err != nil {
		return err
	}

	machDir := config.MachineDir()
	binDir := config.BinDir()
	pidFile := filepath.Join(machDir, "vfkit.pid")

	if running, _ := VfkitRunning(pidFile); running {
		return errors.New("flowcraft VM is already running")
	}

	vfkitBin, err := EnsureVfkit(ctx, binDir, nil)
	if err != nil {
		return err
	}

	diskPath, err := v.DiskResolver(ctx, v.Version)
	if err != nil {
		return fmt.Errorf("ensure VM image: %w", err)
	}

	firstBoot := !IsProvisioned()
	if firstBoot {
		if err := growDisk(diskPath, 20*1024*1024*1024); err != nil {
			return fmt.Errorf("grow disk: %w", err)
		}
	}

	if v.BinaryResolver != nil {
		if _, err := v.BinaryResolver(ctx, v.Version); err != nil {
			return fmt.Errorf("ensure linux binary: %w", err)
		}
	}

	mac, err := LoadOrCreateMAC(filepath.Join(machDir, "mac-address"))
	if err != nil {
		return fmt.Errorf("load MAC: %w", err)
	}

	vfkitCfg := VfkitConfig{
		DiskPath:     diskPath,
		EFIStorePath: filepath.Join(machDir, "efi-variable-store"),
		LogPath:      filepath.Join(machDir, "vm.log"),
		MACAddress:   mac,
		BinShareDir:  binDir,
		DataShareDir: config.DataDir(),
		PIDFile:      pidFile,
	}

	if firstBoot {
		ciDir := filepath.Join(machDir, "cloud-init")
		ud, md, err := WriteCloudInitFiles(ciDir)
		if err != nil {
			return fmt.Errorf("write cloud-init: %w", err)
		}
		vfkitCfg.CloudInitUserData = ud
		vfkitCfg.CloudInitMetaData = md
	}

	leaseSnapshot := SnapshotVZLeases()

	telemetry.Info(ctx, "vm: starting vfkit")
	if err := StartVfkit(ctx, vfkitBin, vfkitCfg); err != nil {
		return err
	}

	cfg := config.Load()
	port := serverPort(cfg)

	// On reboot, try saved IP directly with the full healthz timeout.
	// The VM needs 40-60s to boot (GRUB + kernel + systemd), so a short
	// timeout for IP probing doesn't work.
	if !firstBoot {
		if savedIP, err := readGuestIP(machDir); err == nil {
			telemetry.Info(ctx, "vm: reboot — trying saved IP", otellog.String("ip", savedIP))
			url := fmt.Sprintf("http://%s:%d/healthz", savedIP, port)
			if err := waitHealthyURL(ctx, url, 120*time.Second); err == nil {
				telemetry.Info(ctx, "vm: FlowCraft is ready (saved IP)", otellog.String("ip", savedIP))
				return nil
			}
			telemetry.Info(ctx, "vm: saved IP unreachable, falling back to lease detection")
		}
	}

	telemetry.Info(ctx, "vm: waiting for network")
	guestIP, err := WaitForGuestIP(ctx, 30*time.Second, leaseSnapshot)
	if err != nil {
		_ = StopVfkit(pidFile, 10*time.Second)
		return fmt.Errorf("resolve guest IP: %w", err)
	}
	telemetry.Info(ctx, "vm: guest IP resolved", otellog.String("ip", guestIP))
	_ = os.WriteFile(filepath.Join(machDir, "guest.ip"), []byte(guestIP), 0o644)

	healthzURL := fmt.Sprintf("http://%s:%d/healthz", guestIP, port)
	if firstBoot {
		telemetry.Info(ctx, "vm: first boot — cloud-init provisioning in progress")
	}
	telemetry.Info(ctx, "vm: waiting for server healthz")
	timeout := 120 * time.Second
	if firstBoot {
		timeout = 5 * time.Minute
	}
	if err := waitHealthyURL(ctx, healthzURL, timeout); err != nil {
		_ = StopVfkit(pidFile, 10*time.Second)
		return fmt.Errorf("healthz: %w", err)
	}
	telemetry.Info(ctx, "vm: FlowCraft is ready")
	return nil
}

func (v *VM) Stop(ctx context.Context) error {
	_ = ctx
	pidFile := filepath.Join(config.MachineDir(), "vfkit.pid")
	return StopVfkit(pidFile, 30*time.Second)
}

// ResetScope mirrors machine.ResetScope to avoid an import cycle.
const (
	ScopeMachine = iota
	ScopeData
	ScopeAll
)

func (v *VM) Reset(ctx context.Context, scope int) error {
	_ = v.Stop(ctx)
	switch scope {
	case ScopeMachine:
		_ = os.RemoveAll(config.MachineDir())
		_ = os.Remove(filepath.Join(config.DataDir(), ".provisioned"))
		return nil
	case ScopeData:
		return os.RemoveAll(config.DataDir())
	default:
		return os.RemoveAll(config.HomeRoot())
	}
}

func (v *VM) GetStatus(ctx context.Context) (*Status, error) {
	machDir := config.MachineDir()
	pidFile := filepath.Join(machDir, "vfkit.pid")
	running, pid := VfkitRunning(pidFile)
	st := &Status{Running: running, PID: pid}
	if !running {
		return st, nil
	}

	guestIP, err := readGuestIP(machDir)
	if err != nil {
		return st, nil
	}

	cfg := config.Load()
	port := serverPort(cfg)
	url := fmt.Sprintf("http://%s:%d/healthz", guestIP, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return st, nil
	}
	resp, err := healthzClient.Do(req)
	if err != nil {
		return st, nil
	}
	defer resp.Body.Close()
	st.HealthzOK = resp.StatusCode == http.StatusOK
	return st, nil
}

func (v *VM) OpenWeb(ctx context.Context) error {
	machDir := config.MachineDir()
	guestIP, err := readGuestIP(machDir)
	if err != nil {
		return errors.New("server is not running (no guest IP)")
	}
	cfg := config.Load()
	port := serverPort(cfg)
	url := fmt.Sprintf("http://%s:%d", guestIP, port)
	return exec.CommandContext(ctx, "open", url).Run()
}

func (v *VM) Logs(_ context.Context, w io.Writer) error {
	logPath := filepath.Join(config.MachineDir(), "vm.log")
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

var healthzClient = &http.Client{Timeout: 5 * time.Second}

func waitHealthyURL(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := healthzClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return errors.New("timeout waiting for VM healthz")
}

func readGuestIP(machDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(machDir, "guest.ip"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func serverPort(cfg *config.Config) int {
	_, portStr, err := net.SplitHostPort(cfg.Address())
	if err != nil {
		return 8080
	}
	p, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return 8080
	}
	return p
}

func growDisk(path string, size int64) error {
	return os.Truncate(path, size)
}
