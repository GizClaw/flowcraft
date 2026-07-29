//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package yaml

// syncDir is a no-op on platforms where directory handles cannot be flushed
// the POSIX way — notably Windows, where FlushFileBuffers on a read-only
// directory handle fails. The per-file Sync before rename remains the
// primary durability barrier there.
func syncDir(string) error { return nil }
