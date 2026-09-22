//go:build linux

package sandbox

import "golang.org/x/sys/unix"

// sigintNumber is SIGINT as the kernel numbers signals. The mask helpers
// take it in that form so their signatures stay platform-independent.
const sigintNumber = 2

// sigset is one thread's signal mask.
type sigset = unix.Sigset_t

// emptySigset is the mask with every signal unblocked.
func emptySigset() sigset { return sigset{} }

// sigsetOnly returns a mask that blocks one signal and nothing else.
func sigsetOnly(sig int) sigset {
	var set sigset
	set.Val[(sig-1)/64] |= 1 << uint((sig-1)%64)
	return set
}

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
