//go:build darwin

package sandbox

import (
	"syscall"
	"unsafe"
)

// _SIG_SETMASK is the Darwin spelling of the mask replacement command
// (BSD numbers it 3, Linux numbers it 2).
const _SIG_SETMASK = 3

// sigset is Darwin's classic 32-bit mask: bit n-1 stands for signal n.
type sigset uint32

// emptySigset is the mask with every signal unblocked.
func emptySigset() sigset { return 0 }

// replaceThreadSignalMask sets the calling thread's mask and returns the
// previous one.
//
// It has to be __pthread_sigmask (329), never sigprocmask (48): XNU ends
// SIG_SETMASK on 48 in set_procsigmask(), which walks every thread in the
// process and writes the same mask to each. Clearing through that would
// unblock signals on unrelated threads, and restoring through it would
// broadcast the spawner's mask onto them - the interrupt-killing mask this
// seam exists to keep off the child, handed to the parent instead.
// __pthread_sigmask touches current_uthread() only, which is why Go's own
// runtime reaches for pthread_sigmask rather than sigprocmask
// (runtime/sys_darwin.go). x/sys/unix wraps the call for Linux only, so
// the syscall goes out directly here.
func replaceThreadSignalMask(set sigset) (sigset, error) {
	var previous sigset
	if _, _, errno := syscall.Syscall(syscall.SYS___PTHREAD_SIGMASK,
		uintptr(_SIG_SETMASK), uintptr(unsafe.Pointer(&set)),
		uintptr(unsafe.Pointer(&previous))); errno != 0 {
		return 0, errno
	}
	return previous, nil
}
