//go:build unix

package server

import (
	"errors"

	"golang.org/x/sys/unix"
)

// isUnsupportedSocketError reports whether a listen failure is the platform
// refusing AF_UNIX outright — the one case an operator can act on, and the
// one case worth naming precisely instead of passing the syscall error up.
func isUnsupportedSocketError(err error) bool {
	return errors.Is(err, unix.EAFNOSUPPORT) ||
		errors.Is(err, unix.EPROTONOSUPPORT)
}
