//go:build unix

package journal

import (
	"os"
	"syscall"
)

// fileIdentity extracts the device and inode of a directory entry. They
// are hints for a consumer's own de-duplication, not durable keys: the
// same file is reachable under several paths, and a device number
// changes when a filesystem is remounted.
func fileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	if info == nil {
		return 0, 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

// fileIdentityAvailable reports whether the stat structures this build
// compiles against carry an inode/device pair, which is what
// sandbox.JournalCapabilities.FileIdentity states. It is a platform
// fact, so it lives next to the code that answers it.
func fileIdentityAvailable() bool { return true }
