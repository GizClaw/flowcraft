//go:build linux

package sandbox

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// sigsetWordBits is the width of one Sigset_t word: linux/386 and
// linux/arm carry 32-bit words where the 64-bit architectures carry 64.
func sigsetWordBits() int {
	var set sigset
	return int(unsafe.Sizeof(set.Val[0]) * 8)
}

// sigsetOnly returns a mask that blocks one signal and nothing else.
func sigsetOnly(sig int) sigset {
	var set sigset
	set.Val[(sig-1)/sigsetWordBits()] |= 1 << uint((sig-1)%sigsetWordBits())
	return set
}

// sigsetBlocks reports whether the mask blocks one signal.
func sigsetBlocks(set sigset, sig int) bool {
	return set.Val[(sig-1)/sigsetWordBits()]&(1<<uint((sig-1)%sigsetWordBits())) != 0
}

// blockThreadSignal blocks one signal on the calling thread, through the
// kernel primitive directly. It deliberately does not go through
// replaceThreadSignalMask: a test that plants its expectations with the
// helper it is checking cannot tell a thread-scoped write from one that
// reaches every thread.
func blockThreadSignal(sig int) error {
	set := sigsetOnly(sig)
	return unix.PthreadSigmask(unix.SIG_SETMASK, &set, nil)
}

// queryThreadSignalMask reads the calling thread's mask without changing
// it: with a nil set the kernel only reports the old mask.
func queryThreadSignalMask() (sigset, error) {
	var current sigset
	if err := unix.PthreadSigmask(unix.SIG_SETMASK, nil, &current); err != nil {
		return sigset{}, err
	}
	return current, nil
}

// currentThreadID names the calling thread, so a test can check that a
// pinned goroutine stayed on its thread.
func currentThreadID() int { return unix.Gettid() }
