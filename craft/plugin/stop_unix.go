//go:build unix

package plugin

import (
	"os"
	"syscall"
)

// signalStop asks one plugin process to exit. SIGTERM is the polite request:
// the plugin has already been told to drain and its connection is closed, so
// this is the backstop for a process that did not take the hint.
func signalStop(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}
