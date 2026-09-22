//go:build darwin

package sandbox

import (
	"syscall"
	"unsafe"
)

// sigintNumber is SIGINT as the kernel numbers signals. The mask helpers
// take it in that form so their signatures stay platform-independent.
const sigintNumber = 2

// _SIG_SETMASK is the Darwin spelling of the mask replacement command
// (BSD numbers it 3, Linux numbers it 2).
const _SIG_SETMASK = 3

// sigset is Darwin's classic 32-bit mask: bit n-1 stands for signal n.
// x/sys/unix wraps pthread_sigmask for Linux only, so the syscall goes
// out directly here.
type sigset uint32

// emptySigset is the mask with every signal unblocked.
func emptySigset() sigset { return 0 }

// sigsetOnly returns a mask that blocks one signal and nothing else.
func sigsetOnly(sig int) sigset { return 1 << uint(sig-1) }

// replaceThreadSignalMask sets the calling thread's mask and returns the
// previous one. A mask belongs to the thread, not the process, so callers
// that care which thread forks must pin themselves first (see
// startWithCleanSignalMask).
func replaceThreadSignalMask(set sigset) (sigset, error) {
	var previous sigset
	if _, _, errno := syscall.Syscall(syscall.SYS_SIGPROCMASK,
		uintptr(_SIG_SETMASK), uintptr(unsafe.Pointer(&set)),
		uintptr(unsafe.Pointer(&previous))); errno != 0 {
		return 0, errno
	}
	return previous, nil
}
