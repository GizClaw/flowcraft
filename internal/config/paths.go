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

// DataDir returns ~/.flowcraft/data.
func DataDir() string {
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

// ServerLogFile returns the path to the server log file.
func ServerLogFile() string {
	return filepath.Join(LogsDir(), "server.log")
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
