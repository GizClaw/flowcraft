package fsx

import (
	"errors"
	"io/fs"
	"syscall"
)

// ErrDenied reports that the boundary refused a path.
//
// It is returned when the path falls outside every root of its side (a read
// outside the read roots, a write without a write grant), and also when the
// kernel refuses an operation inside a root (a read-only mount, a seatbelt deny
// rule, an ACL). Callers should treat it as "the boundary said no" and keep
// [errors.Is] reachable, because the layer that renders an error to a model
// needs to distinguish "outside your boundary" from "the file is missing".
var ErrDenied = errors.New("fsx: path refused")

// ErrClosed reports an operation on a Tree that has already been closed. It is
// distinct from [ErrDenied] on purpose: a denial is a boundary decision, while a
// closed tree is a lifecycle bug in the host.
var ErrClosed = errors.New("fsx: tree is closed")

// isDenied reports whether the kernel refused the operation rather than the
// operation failing. EROFS covers a read-only bind mount, EPERM/EACCES cover a
// seatbelt rule and an ACL.
func isDenied(err error) bool {
	if err == nil {
		return false
	}
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

// denied labels a kernel refusal so the caller sees the boundary's name rather
// than a raw EACCES from wherever the operation happened to fail.
func denied(err error) error {
	if !isDenied(err) {
		return err
	}
	return errors.Join(ErrDenied, err)
}
