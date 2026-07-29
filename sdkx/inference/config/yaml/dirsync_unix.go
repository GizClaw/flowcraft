//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package yaml

import "os"

// syncDir flushes the directory entry created by the atomic rename so the
// new file name survives a crash.
func syncDir(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
