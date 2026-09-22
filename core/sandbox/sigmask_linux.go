//go:build linux

package sandbox

import "golang.org/x/sys/unix"

// sigset is one thread's signal mask.
type sigset = unix.Sigset_t

// emptySigset is the mask with every signal unblocked.
func emptySigset() sigset { return sigset{} }

// replaceThreadSignalMask sets the calling thread's mask and returns the
// previous one. A mask belongs to the thread, not the process, so callers
// that care which thread forks must pin themselves first (see
// startWithCleanSignalMask).
func replaceThreadSignalMask(set sigset) (sigset, error) {
	var previous sigset
	if err := unix.PthreadSigmask(unix.SIG_SETMASK, &set, &previous); err != nil {
		return sigset{}, err
	}
	return previous, nil
}
