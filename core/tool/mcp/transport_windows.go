//go:build windows

package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/core/telemetry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	otellog "go.opentelemetry.io/otel/log"
	xwin "golang.org/x/sys/windows"
)

// defaultMCPShutdownGrace bounds each graceful-shutdown wait on
// Windows: how long to wait after closing stdin, and again after
// sending CTRL+BREAK, before escalating to Kill. It mirrors the SDK's
// default TerminateDuration and is mutable for tests.
var defaultMCPShutdownGrace = 5 * time.Second

// connect spawns the child in its own process group and wires the MCP
// framing over pipes. Windows has no SIGTERM, so the SDK's
// CommandTransport would skip the signal step entirely and jump from
// stdin-close straight to Kill; this transport owns the lifecycle
// instead and delivers CTRL+BREAK as the graceful shutdown signal.
func (t *reconnectableStdio) connect(ctx context.Context) (mcpsdk.Connection, error) {
	cmd := t.newCommand()
	// CREATE_NEW_PROCESS_GROUP makes the child the leader of its own
	// process group, which is what GenerateConsoleCtrlEvent targets.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: xwin.CREATE_NEW_PROCESS_GROUP,
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: windows stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return nil, fmt.Errorf("mcp: windows stdout pipe: %w", err)
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("mcp: start %s: %w", cmd.Path, err)
	}
	// The child holds duplicates of the inheritable pipe ends; the
	// parent-side copies are ours to close.
	_ = stdinR.Close()
	_ = stdoutW.Close()

	// The SDK's IOTransport provides the JSON-RPC framing; Close of
	// the connection it returns only closes the pipes, so the process
	// lifecycle stays in windowsStdioConn.
	inner, err := (&mcpsdk.IOTransport{
		Reader: stdoutR,
		Writer: nopCloser{stdinW},
	}).Connect(ctx)
	if err != nil {
		_ = stdinW.Close()
		_ = stdoutR.Close()
		// The child is already running; do not orphan it. Kill and
		// reap before returning so the MCP server cannot outlive the
		// failed connect attempt and the process handle is released.
		if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			telemetry.WarnErr(context.Background(),
				"mcp: kill child after connect failure", kerr)
		}
		// Reap the child; its (expected) non-zero exit status is the
		// outcome of the kill, not a cleanup failure.
		_ = cmd.Wait()
		return nil, err
	}
	return &windowsStdioConn{Connection: inner, cmd: cmd, stdin: stdinW}, nil
}

// nopCloser forwards writes but treats Close as a no-op: the real
// stdin close happens exactly once in windowsStdioConn.shutdown, so
// the SDK's pipe close cannot double-close it.
type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }

// windowsStdioConn wraps the SDK connection with the Windows process
// lifecycle: close stdin, wait, CTRL+BREAK, wait, Kill.
type windowsStdioConn struct {
	mcpsdk.Connection
	cmd   *exec.Cmd
	stdin io.WriteCloser

	closeOnce sync.Once
	closeErr  error
	waitOnce  sync.Once
	waitDone  chan error
}

// Close implements Connection. It is idempotent and safe to call
// concurrently with Read (the SDK also calls it when a Read fails).
func (c *windowsStdioConn) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.shutdown() })
	return c.closeErr
}

// shutdown mirrors the MCP stdio spec's escalation, substituting
// CTRL+BREAK for SIGTERM (Windows has no portable SIGTERM delivery):
// close stdin, wait for a graceful exit, signal, wait again, then
// Kill. CTRL+BREAK only reaches a process group that shares a console
// with the caller, so on console-less hosts it fails and the child is
// killed after the second grace — the same outcome as before this
// transport, minus the lost signal step.
func (c *windowsStdioConn) shutdown() error {
	stdinErr := c.stdin.Close()

	waitErr, exited := c.wait(defaultMCPShutdownGrace)
	if !exited {
		if err := xwin.GenerateConsoleCtrlEvent(
			xwin.CTRL_BREAK_EVENT, uint32(c.cmd.Process.Pid)); err != nil {
			// Console-less hosts cannot receive CTRL+BREAK; the child
			// is killed after the second grace instead. Surface the
			// dropped signal so the degradation is observable.
			telemetry.WarnErr(context.Background(),
				"mcp: ctrl-break to child failed, escalating to kill", err,
				otellog.Int("mcp.pid", c.cmd.Process.Pid))
		}
		waitErr, exited = c.wait(defaultMCPShutdownGrace)
		if !exited {
			if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				waitErr = errors.Join(waitErr, err)
			} else {
				waitErr, _ = c.wait(defaultMCPShutdownGrace)
			}
		}
	}
	// Close the SDK framing, releasing the stdout read pipe. The
	// writer side is a nopCloser, so stdin is not double-closed.
	if err := c.Connection.Close(); err != nil {
		telemetry.WarnErr(context.Background(),
			"mcp: close sdk framing failed", err)
	}
	return errors.Join(stdinErr, waitErr)
}

// wait blocks until the child exits or grace elapses. cmd.Wait is
// started exactly once; timed-out waits leave the goroutine blocked
// until the eventual kill unblocks it.
func (c *windowsStdioConn) wait(grace time.Duration) (error, bool) {
	c.waitOnce.Do(func() {
		c.waitDone = make(chan error, 1)
		go func() { c.waitDone <- c.cmd.Wait() }()
	})
	select {
	case err := <-c.waitDone:
		return err, true
	case <-time.After(grace):
		return nil, false
	}
}
