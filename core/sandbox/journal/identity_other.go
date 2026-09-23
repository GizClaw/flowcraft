//go:build !unix

package journal

import "os"

// fileIdentity reports nothing on platforms whose stat structures carry
// no inode/device pair: the fields stay zero and
// sandbox.JournalCapabilities.FileIdentity is false. Windows is the
// platform this build is for today: its identity is a volume serial
// number plus a file index reached through the file's own handle, which
// is a different call with different lifetime rules, not this one.
func fileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	return 0, 0, false
}

// fileIdentityAvailable is the platform fact behind
// sandbox.JournalCapabilities.FileIdentity, and it is the same answer
// as the one above: nothing fills Dev/Ino here.
func fileIdentityAvailable() bool { return false }
