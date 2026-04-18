package darwin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

// VfkitConfig describes the VM resources and paths.
type VfkitConfig struct {
	CPUs   int
	Memory int // MiB

	DiskPath     string
	EFIStorePath string
	LogPath      string
	MACAddress   string // fixed MAC for stable DHCP leases across reboots

	BinShareDir  string // host dir → /opt/flowcraft
	DataShareDir string // host dir → /data

	CloudInitUserData string // path to cloud-init user-data
	CloudInitMetaData string // path to cloud-init meta-data

	PIDFile    string // records vfkit PID
	SocketPath string // UNIX socket for vfkit REST API
}

func (c *VfkitConfig) defaults() {
	if c.CPUs <= 0 {
		c.CPUs = 2
	}
	if c.Memory <= 0 {
		c.Memory = 4096
	}
	if c.SocketPath == "" {
		c.SocketPath = filepath.Join(filepath.Dir(c.PIDFile), "vfkit.sock")
	}
}

// StartVfkit launches a vfkit process in the background and records its PID.
func StartVfkit(ctx context.Context, vfkitBin string, cfg VfkitConfig) error {
	cfg.defaults()
	_ = os.Remove(cfg.SocketPath) // remove stale socket

	// Use "create" only if the EFI store doesn't exist yet; reuse on reboots
	// to preserve UEFI boot entries.
	efiOpt := fmt.Sprintf("efi,variable-store=%s", cfg.EFIStorePath)
	if _, err := os.Stat(cfg.EFIStorePath); os.IsNotExist(err) {
		efiOpt += ",create"
	}

	netDev := "virtio-net,nat"
	if cfg.MACAddress != "" {
		netDev += ",mac=" + cfg.MACAddress
	}

	args := []string{
		"--cpus", strconv.Itoa(cfg.CPUs),
		"--memory", strconv.Itoa(cfg.Memory),
		"--bootloader", efiOpt,
		"--device", fmt.Sprintf("virtio-blk,path=%s", cfg.DiskPath),
		"--device", fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=bin", cfg.BinShareDir),
		"--device", fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=data", cfg.DataShareDir),
		"--device", netDev,
		"--device", fmt.Sprintf("virtio-serial,logFilePath=%s", cfg.LogPath),
		"--restful-uri", fmt.Sprintf("unix://%s", cfg.SocketPath),
	}
	if cfg.CloudInitUserData != "" && cfg.CloudInitMetaData != "" {
		args = append(args, "--cloud-init",
			fmt.Sprintf("%s,%s", cfg.CloudInitUserData, cfg.CloudInitMetaData))
	}

	logf, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open vm log: %w", err)
	}

	cmd := exec.CommandContext(ctx, vfkitBin, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("start vfkit: %w", err)
	}
	logf.Close()

	pid := cmd.Process.Pid
	if err := os.WriteFile(cfg.PIDFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return fmt.Errorf("write pid file: %w", err)
	}

	// Verify vfkit didn't exit immediately (e.g. disk already locked).
	time.Sleep(2 * time.Second)
	if err := syscall.Kill(pid, 0); err != nil {
		logData, _ := os.ReadFile(cfg.LogPath)
		_ = os.Remove(cfg.PIDFile)
		return fmt.Errorf("vfkit exited immediately; log:\n%s", string(logData))
	}

	return nil
}

// StopVfkit requests a graceful VM shutdown via the REST API (ACPI power
// button), waits for the guest to shut down, then falls back to SIGTERM/SIGKILL.
func StopVfkit(pidFile string, timeout time.Duration) error {
	pid, err := readPID(pidFile)
	if err != nil {
		return err
	}

	if err := syscall.Kill(pid, 0); err != nil {
		_ = os.Remove(pidFile)
		return nil
	}

	sockPath := filepath.Join(filepath.Dir(pidFile), "vfkit.sock")
	_ = requestStopViaSocket(sockPath)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			_ = os.Remove(pidFile)
			_ = os.Remove(sockPath)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	_ = syscall.Kill(pid, syscall.SIGTERM)
	time.Sleep(2 * time.Second)

	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = os.Remove(pidFile)
	_ = os.Remove(sockPath)
	return nil
}

// VfkitRunning checks if the vfkit process recorded in pidFile is alive.
func VfkitRunning(pidFile string) (bool, int) {
	pid, err := readPID(pidFile)
	if err != nil {
		return false, 0
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false, pid
	}
	return true, pid
}

// VfkitState queries the vfkit REST API for VM state via the UNIX socket.
func VfkitState(sockPath string) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("http://localhost/vm/state")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var state struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return "", err
	}
	return state.State, nil
}

func requestStopViaSocket(sockPath string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	body := bytes.NewBufferString(`{"state":"Stop"}`)
	req, err := http.NewRequest(http.MethodPost, "http://localhost/vm/state", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("vfkit API returned %d", resp.StatusCode)
	}
	return nil
}

// vzHWPrefix is the hw_address prefix used by Apple Virtualization.framework
// VMs in /var/db/dhcpd_leases. Non-VZ devices (Docker, etc.) use shorter
// "1,xx:xx:xx:xx:xx:xx" format and must be excluded.
const vzHWPrefix = "ff,f1:f5:dd:7f:"

// SnapshotVZLeases returns VZ hw_address→lease mappings from the DHCP leases
// file. Call before starting vfkit so we can detect both new VMs (new
// hw_address) and rebooted VMs (same hw_address, renewed lease timestamp).
func SnapshotVZLeases() map[string]string {
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return make(map[string]string)
	}
	m := make(map[string]string)
	var hw, lease string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "hw_address="):
			hw = strings.TrimPrefix(line, "hw_address=")
		case strings.HasPrefix(line, "lease="):
			lease = strings.TrimPrefix(line, "lease=")
		case line == "}":
			if strings.HasPrefix(hw, vzHWPrefix) {
				m[hw] = lease
			}
			hw, lease = "", ""
		}
	}
	return m
}

// WaitForGuestIP polls DHCP leases until a VZ entry appears that is either
// brand new (hw_address not in snapshot) or renewed (same hw_address but
// larger lease timestamp). This handles both first-boot and reboot scenarios.
func WaitForGuestIP(ctx context.Context, timeout time.Duration, snapshot map[string]string) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ip := findChangedVZLease(snapshot); ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("timeout waiting for guest IP")
}

func findChangedVZLease(snapshot map[string]string) string {
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}

	var currentIP, currentHW, currentLease string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ip_address="):
			currentIP = strings.TrimPrefix(line, "ip_address=")
		case strings.HasPrefix(line, "hw_address="):
			currentHW = strings.TrimPrefix(line, "hw_address=")
		case strings.HasPrefix(line, "lease="):
			currentLease = strings.TrimPrefix(line, "lease=")
		case line == "}":
			if currentIP != "" && strings.HasPrefix(currentHW, vzHWPrefix) {
				prevLease, known := snapshot[currentHW]
				if !known || currentLease != prevLease {
					return currentIP
				}
			}
			currentIP, currentHW, currentLease = "", "", ""
		}
	}
	return ""
}

func readPID(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("vfkit not running (no pid file)")
		}
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid vfkit pid file")
	}
	return pid, nil
}

// ResolveVfkit finds the vfkit binary, checking the bundled path first,
// then falling back to PATH.
func ResolveVfkit(bundledPath string) (string, error) {
	if _, err := os.Stat(bundledPath); err == nil {
		return bundledPath, nil
	}
	p, err := exec.LookPath("vfkit")
	if err != nil {
		return "", fmt.Errorf("vfkit not found at %s or in PATH; install via: brew install vfkit", bundledPath)
	}
	return p, nil
}

// LoadOrCreateMAC returns a stable MAC address, persisted to macFile.
// On first call it generates a random locally-administered MAC and saves it.
func LoadOrCreateMAC(macFile string) (string, error) {
	if data, err := os.ReadFile(macFile); err == nil {
		if mac := strings.TrimSpace(string(data)); mac != "" {
			return mac, nil
		}
	}
	mac := generateMAC()
	if err := os.WriteFile(macFile, []byte(mac+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("save MAC: %w", err)
	}
	return mac, nil
}

func generateMAC() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	// Set locally-administered, unicast bits.
	b[0] = (b[0] | 0x02) & 0xFE
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// EnsureVfkit downloads vfkit if not already available.
// Returns the path to the vfkit binary.
func EnsureVfkit(ctx context.Context, binDir string, downloadFn func(ctx context.Context, dst string) error) (string, error) {
	bundled := filepath.Join(binDir, "vfkit")
	path, err := ResolveVfkit(bundled)
	if err == nil {
		return path, nil
	}

	if downloadFn == nil {
		return "", fmt.Errorf("vfkit not found; install via: brew install vfkit")
	}

	telemetry.Info(ctx, "vm: downloading vfkit")
	if err := downloadFn(ctx, bundled); err != nil {
		return "", fmt.Errorf("download vfkit: %w", err)
	}
	if err := os.Chmod(bundled, 0o755); err != nil {
		return "", err
	}
	return bundled, nil
}
