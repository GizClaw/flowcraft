package config

import (
	"os"
	"path/filepath"
)

// HomeRoot returns the FlowCraft home directory (~/.flowcraft under the
// current user's home). Configuration lives here; it is not overridden
// by environment variables.
//
// If [os.UserHomeDir] fails (typically because HOME is unset — e.g. a
// systemd unit started without Environment=HOME=...) the result falls
// back to a directory under [os.TempDir]. Callers that want to detect
// this degraded mode should consult [HomeRootDegraded]; long-running
// processes should log a prominent warning so it is not silently
// masked: writes that should have been persistent will end up on tmpfs
// and disappear on the next reboot.
func HomeRoot() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".flowcraft")
	}
	return filepath.Join(os.TempDir(), "flowcraft")
}

// HomeRootDegraded reports whether [HomeRoot] would fall back to a
// temporary directory because the user's home directory cannot be
// determined. In this state any path derived from HomeRoot (config.yaml,
// PID file, crash log, machine state) lives on ephemeral storage.
// [DataDir] and the paths derived from it ([WorkspaceDir],
// [CheckpointsDir], [Config.DBPath], [Config.LogFilePath]) are
// unaffected because they can be pinned with FLOWCRAFT_DATA_DIR.
func HomeRootDegraded() bool {
	_, err := os.UserHomeDir()
	return err != nil
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

// WorkspaceDir returns the runtime workspace root: agent sandboxes,
// long-term memory, knowledge base, skills, and plugins. Lives under
// [DataDir] because every file written here is user-generated state
// that must survive a VM restart on macOS — DataDir is the
// virtio-fs-shared mount with the host, while HomeRoot inside the
// guest is on tmpfs and gets wiped on reboot.
func WorkspaceDir() string {
	return filepath.Join(DataDir(), "workspace")
}

// CheckpointsDir returns the directory where the graph executor stores
// per-run checkpoints. Lives under [DataDir] for the same reason as
// [WorkspaceDir].
func CheckpointsDir() string {
	return filepath.Join(DataDir(), "checkpoints")
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
