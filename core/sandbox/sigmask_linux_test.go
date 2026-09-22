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

// sigsetOnly returns a mask that blocks exactly the given signals and
// nothing else.
func sigsetOnly(sigs ...int) sigset {
	var set sigset
	for _, sig := range sigs {
		set.Val[(sig-1)/sigsetWordBits()] |= 1 << uint((sig-1)%sigsetWordBits())
	}
	return set
}

// sigsetBlocks reports whether the mask blocks one signal.
func sigsetBlocks(set sigset, sig int) bool {
	return set.Val[(sig-1)/sigsetWordBits()]&(1<<uint((sig-1)%sigsetWordBits())) != 0
}

// swapThreadSignalMask replaces the calling thread's mask with set, through
// the kernel primitive directly, and returns the mask it replaced. It
// deliberately does not go through replaceThreadSignalMask: a test that
// plants its expectations with the helper it is checking cannot tell a
// thread-scoped write from one that reaches every thread. Handing the
// previous mask back is what lets a caller put the thread right before it
// unpins.
func swapThreadSignalMask(set sigset) (sigset, error) {
	var previous sigset
	if err := unix.PthreadSigmask(unix.SIG_SETMASK, &set, &previous); err != nil {
		return sigset{}, err
	}
	return previous, nil
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
