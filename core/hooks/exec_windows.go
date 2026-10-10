//go:build windows

package hooks

import (
	"os"
	"syscall"
)

// processGroupAttr puts the command in a new process group. Windows
// consoles have no process-group kill, so this is bookkeeping rather
// than a capability: it keeps the hook out of the parent's group.
func processGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup kills the command itself, and only the command: a
// Windows process cannot be killed as part of a group without a job
// object, so descendants a hook spawned may outlive the timeout. That
// is the documented difference from the Unix half, where the whole
// group dies; it is best-effort, like the rest of a hook failure.
//
// The shell this package runs commands under (sh -c) is the Unix
// convention too: on Windows the environment must provide one, as
// development environments that ship git-bash do.
func killProcessGroup(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	return proc.Kill()
}
