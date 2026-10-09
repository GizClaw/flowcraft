//go:build unix

package hooks

import (
	"errors"
	"os"
	"syscall"
)

// processGroupAttr starts the command as the leader of a new process
// group, which is what makes a group kill possible.
func processGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the command's whole process group. SIGKILL
// rather than SIGTERM: the hook is already over budget, and a script
// that traps TERM would otherwise get a second grace period it was
// never granted.
func killProcessGroup(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	// The group id equals the leader's pid, because the command was
	// started with Setpgid.
	if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
