//go:build windows

package plugin

import "os"

// signalStop ends one plugin process. Windows has no SIGTERM, and Go maps
// os.Interrupt to nothing for a child it did not create a console for, so
// terminating is the only request that arrives.
func signalStop(process *os.Process) error {
	return process.Kill()
}
