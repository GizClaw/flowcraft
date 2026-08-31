//go:build !windows

package workspace

// removeWithRetry is a pass-through on platforms where unlink works
// even while the target is open, so the Windows retry loop never runs
// on unix.
func removeWithRetry(op func() error) error {
	return op()
}
