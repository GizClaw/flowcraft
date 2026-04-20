package config

import (
	"os"
	"path/filepath"
)

// HomeRoot returns the FlowCraft home directory (~/.flowcraft under the current user's home).
// Configuration lives here; it is not overridden by environment variables.
func HomeRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "flowcraft")
	}
	return filepath.Join(home, ".flowcraft")
}

// ConfigFile returns the path to config.yaml.
func ConfigFile() string {
	return filepath.Join(HomeRoot(), "config.yaml")
}

// BinDir returns ~/.flowcraft/bin.
func BinDir() string {
	return filepath.Join(HomeRoot(), "bin")
}

// DataDir returns the FlowCraft data directory.
//
// Defaults to ~/.flowcraft/data; can be overridden by setting the
// FLOWCRAFT_DATA_DIR environment variable, which the macOS VM uses to
// point at /data (a virtio-fs mount of the host's DataDir). This is the
// only path that is env-overridable so that host-shared state — the
// database, structured log file, plugins data — lands on the host disk.
func DataDir() string {
	if v := os.Getenv("FLOWCRAFT_DATA_DIR"); v != "" {
		return v
	}
	return filepath.Join(HomeRoot(), "data")
}

// LogsDir returns ~/.flowcraft/logs.
func LogsDir() string {
	return filepath.Join(HomeRoot(), "logs")
}

// MachineDir returns ~/.flowcraft/machine.
func MachineDir() string {
	return filepath.Join(HomeRoot(), "machine")
}

// PIDFile returns the server PID file path.
func PIDFile() string {
	return filepath.Join(HomeRoot(), "server.pid")
}

// ServerCrashLogFile returns the path to the server stdout/stderr capture
// file. This sink catches panics and any output not routed through the OTel
// log pipeline; it is single-file (no rotation) since crash output is rare
// and small. For structured application logs use [Config.LogFilePath]
// instead.
func ServerCrashLogFile() string {
	return filepath.Join(LogsDir(), "server.crash.log")
}

// EnsureLayout creates ~/.flowcraft subdirectories if missing.
func EnsureLayout() error {
	for _, d := range []string{HomeRoot(), BinDir(), DataDir(), LogsDir(), MachineDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
