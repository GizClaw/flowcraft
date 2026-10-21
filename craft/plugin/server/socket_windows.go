//go:build windows

package server

import (
	"errors"
	"net"

	"golang.org/x/sys/windows"
)

// isUnsupportedSocketError reports whether a listen failure is Windows
// refusing AF_UNIX. Builds older than 17063 have no support for it at all,
// and the net package reports that either as the winsock error or as an
// unknown network, depending on which probe failed first.
func isUnsupportedSocketError(err error) bool {
	if errors.Is(err, windows.WSAEAFNOSUPPORT) ||
		errors.Is(err, windows.WSAEPROTONOSUPPORT) {
		return true
	}
	var unknown net.UnknownNetworkError
	return errors.As(err, &unknown) && string(unknown) == "unix"
}
