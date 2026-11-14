package fsx

import (
	"errors"
	"io/fs"
	"syscall"
)

// isDenied reports whether the kernel refused the operation rather than the
// operation failing. EROFS covers a read-only bind mount, EPERM/EACCES cover a
// seatbelt rule and an ACL.
func isDenied(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EROFS, syscall.EPERM, syscall.EACCES:
			return true
		}
	}
	return false
}
