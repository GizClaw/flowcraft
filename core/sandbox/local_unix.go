//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
	"time"
)

// applyProcAttrs places the child in its own process group and arranges
// for the whole group — not just the leader — to be killed when ctx is
// done, so orphaned descendants cannot keep burning CPU after a timeout.
// WaitDelay bounds how long output copying may outlive the kill before
// the pipes are force-closed (only reachable if a descendant escaped the
// group via setpgid/daemonize).
func applyProcAttrs(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// The child leads its own group, so its pid equals the pgid.
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = 2 * time.Second
}
