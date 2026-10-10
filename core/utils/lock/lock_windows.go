//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffset is the byte the exclusive range lock covers: far past
// everything this package writes, because the holder record lives at
// offset 0 and a Windows byte-range lock is *enforced*, not advisory.
//
// That difference is the whole reason this constant exists. On Unix a
// flock is a hint: the kernel keeps other processes out of the lock but
// they can still read the file, so the record written by the holder is
// readable by exactly the process trying to ask who holds it. Locking
// offset 0 on Windows instead makes every read of the record fail with
// ERROR_LOCK_VIOLATION — the busy path would then report "held by another
// live process" with an empty holder, and a second handle in the same
// process would stop being recognized as ours.
//
// 1 GiB is arbitrary and deliberate: no record this package writes gets
// near it, and it stays inside the low 32 bits of the OVERLAPPED offset
// so the byte arithmetic is obvious.
const lockOffset = 1 << 30

// lockFile takes an exclusive byte-range lock without blocking. A range
// lock (instead of a whole-file lock) keeps the file readable for the
// diagnostic read that follows a busy answer — as long as the range stays
// clear of the record (see lockOffset).
func lockFile(file *os.File) error {
	overlapped := windows.Overlapped{Offset: lockOffset}
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
}

// unlockFile drops the range lock.
func unlockFile(file *os.File) error {
	overlapped := windows.Overlapped{Offset: lockOffset}
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

// isLockBusy reports whether err means "another holder has it".
func isLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_IO_PENDING)
}
