package server

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// maxSocketPath bounds a unix socket path. sockaddr_un.sun_path is 104
// bytes on the BSDs and 108 on Linux, and Windows keeps AF_UNIX paths to
// the same limit, so the shortest of them is what every platform honors.
const maxSocketPath = 100

// socketNameLen is the length of the socket's own file name: six random
// bytes in hex, and nothing else.
const socketNameLen = 12

// RuntimeDir is where plugin sockets live: short, per-user, and inside a
// directory this process creates with 0700.
//
// The directory is chosen for length as much as for taste. A socket path
// that overruns sun_path fails with a bare "invalid argument", so the name
// craft appends is twelve hex characters and nothing else — a readable
// "<plugin>-<generation>.sock" does not fit once the temp directory itself
// is on a long path, which on Windows it usually is.
func RuntimeDir() (string, error) {
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = os.TempDir()
			return filepath.Join(base, "flowcraft"), nil
		}
		return filepath.Join(base, "Temp", "flowcraft"), nil
	case "darwin":
		base = os.TempDir()
	default:
		if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
			base = dir
			break
		}
		base = os.TempDir()
	}
	return filepath.Join(base, "flowcraft"), nil
}

// socketPath makes one socket path inside dir, creating dir if needed.
func socketPath(dir string) (string, error) {
	if dir == "" {
		return "", errdefs.Validationf("plugin host: socket directory is required")
	}
	// The bound is checked before the directory is created: a directory
	// that cannot hold a socket is not worth making, and a caller who
	// passed one should not be left with an empty directory as the trace
	// of a call that failed.
	if len(filepath.Join(dir, strings.Repeat("0", socketNameLen))) > maxSocketPath {
		return "", errdefs.Validationf(
			"plugin host: socket directory %q is too long: a socket path "+
				"may be %d bytes", dir, maxSocketPath)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errdefs.NotAvailablef(
			"plugin host: create socket directory %s: %w", dir, err)
	}
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errdefs.Internal(err)
	}
	path := filepath.Join(dir, hex.EncodeToString(raw[:]))
	return path, nil
}

// UnsupportedError explains why a host cannot serve plugin sockets here.
// It is a validation class on purpose: the platform cannot be fixed by
// waiting, so an enable fails at once instead of timing out.
func UnsupportedError() error {
	if runtime.GOOS == "windows" {
		return errdefs.Validationf(
			"plugin host: this Windows build has no AF_UNIX support; " +
				"Windows 10 build 17063 or later is required")
	}
	return errdefs.Validationf(
		"plugin host: the platform %q has no unix domain sockets", runtime.GOOS)
}
