//go:build windows

package windows

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/GizClaw/flowcraft/core/errdefs"

	xwin "golang.org/x/sys/windows"
)

// verifyFenceTimeout bounds one behavioral probe (two TCP dials under
// the AppContainer token). PowerShell startup dominates the cost.
const verifyFenceTimeout = 15 * time.Second

// verifyFence behaviorally confirms the WFP fence is effective after
// filter installation. BFE enumeration is restricted to administrators,
// and even an elevated add-filter call does not prove the kernel
// evaluates the filter with the intended precedence, so the probe runs
// real connections under the container token:
//
//   - a dial to a host-side loopback listener that is NOT the proxy
//     port must be blocked (catches a missing or mis-ordered egress
//     block, or an over-broad loopback exemption);
//   - in allow-list / proxy modes a dial to the enforcement proxy port
//     must succeed (without a working permit, the AppIsolation default
//     deny would make the sandbox unusable and the failure would only
//     surface later, inside the sandboxed command).
//
// A mismatch fails closed: the isolation is never handed to a caller
// whose fence cannot be trusted. This mirrors srt-win's `wfp verify`
// (the sandbox-runtime Windows helper) rather than trusting filter
// install success alone.
func (n *netIsolation) verifyFence() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: fence probe listener: %w", err))
	}
	defer func() { _ = ln.Close() }()
	probePort := ln.Addr().(*net.TCPAddr).Port

	// PowerShell is the only guaranteed dial-capable builtin on
	// Windows; the net-policy integration tests already rely on it
	// under the same AppContainer token.
	want := "blocked"
	script := fmt.Sprintf(
		"$r=@(); foreach($p in @(%d)){ try{ $c=New-Object Net.Sockets.TcpClient; $c.Connect('127.0.0.1',$p); $c.Close(); $r+='ok' } catch { $r+='blocked' } }; [string]::Join(',',$r)",
		probePort)
	if n.proxyPort > 0 {
		want = "blocked,ok"
		script = fmt.Sprintf(
			"$r=@(); foreach($p in @(%d,%d)){ try{ $c=New-Object Net.Sockets.TcpClient; $c.Connect('127.0.0.1',$p); $c.Close(); $r+='ok' } catch { $r+='blocked' } }; [string]::Join(',',$r)",
			probePort, n.proxyPort)
	}

	cmd := exec.Command("powershell",
		"-NoProfile", "-NonInteractive", "-EncodedCommand", encodeCommand(script))
	cmd.SysProcAttr = &xwin.SysProcAttr{
		CreationFlags: xwin.CREATE_NO_WINDOW,
		Token:         syscall.Token(n.token),
	}
	cmd.Env = n.env(os.Environ())
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := enableCreateProcessAsUserPrivileges(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: fence probe start: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), verifyFenceTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return errdefs.Internal(fmt.Errorf("windows: fence probe timed out"))
	case err := <-done:
		if err != nil {
			// A probe that cannot run must not be treated as a pass.
			return errdefs.Internal(fmt.Errorf("windows: fence probe exit: %w", err))
		}
	}
	got := strings.TrimSpace(out.String())
	if got != want {
		return errdefs.Internal(fmt.Errorf(
			"windows: WFP fence verify failed: got %q, want %q", got, want))
	}
	return nil
}

// encodeCommand wraps a PowerShell script in an -EncodedCommand blob
// (UTF-16LE base64), avoiding argument-quoting fragility entirely.
func encodeCommand(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[i*2] = byte(v)
		b[i*2+1] = byte(v >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
