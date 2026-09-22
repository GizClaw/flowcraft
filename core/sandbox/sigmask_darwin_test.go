//go:build darwin

package sandbox

import (
	"syscall"
	"unsafe"
)

// sigsetOnly returns a mask that blocks one signal and nothing else.
// Darwin's mask is a single 32-bit word, so bit n-1 stands for signal n.
func sigsetOnly(sig int) sigset { return 1 << uint(sig-1) }

// sigsetBlocks reports whether the mask blocks one signal.
func sigsetBlocks(set sigset, sig int) bool { return set&(1<<uint(sig-1)) != 0 }

// blockThreadSignal blocks one signal on the calling thread, through
// __pthread_sigmask directly. It deliberately does not go through
// replaceThreadSignalMask: a test that plants its expectations with the
// helper it is checking cannot tell a thread-scoped write from one that
// reaches every thread.
func blockThreadSignal(sig int) error {
	set := sigsetOnly(sig)
	if _, _, errno := syscall.Syscall(syscall.SYS___PTHREAD_SIGMASK,
		uintptr(_SIG_SETMASK), uintptr(unsafe.Pointer(&set)), 0); errno != 0 {
		return errno
	}
	return nil
}

// queryThreadSignalMask reads the calling thread's mask without changing
// it: with a nil set __pthread_sigmask only reports the old mask.
func queryThreadSignalMask() (sigset, error) {
	var current sigset
	if _, _, errno := syscall.Syscall(syscall.SYS___PTHREAD_SIGMASK,
		uintptr(_SIG_SETMASK), 0, uintptr(unsafe.Pointer(&current))); errno != 0 {
		return 0, errno
	}
	return current, nil
}

// currentThreadID names the calling thread: Darwin has no gettid(2), but
// thread_selfid is a plain syscall.
func currentThreadID() int {
	id, _, _ := syscall.Syscall(syscall.SYS_THREAD_SELFID, 0, 0, 0)
	return int(id)
}
