package mcp

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// connectError classifies a go-sdk connect failure into an errdefs
// category so callers can branch on it without string matching.
//
// The distinction that matters operationally is "the server is not
// there" versus "the server is there but rejected us", plus the one
// failure retrying can never fix: a command the OS cannot start. The
// first is NotAvailable: a refused dial, a child that died during
// startup — retrying later or fixing configuration may fix it, and a
// host attaching several servers should be able to carry on without
// this one. The second is Validation: initialization completed far
// enough for the peer to say no, which points at the request, not the
// environment. A command that cannot even be spawned — missing, not
// executable, not a runnable file — is Validation too, because the
// retry loop treats that class as terminal: churning against a binary
// that is not there only hides the problem from the host. A cancelled
// or expired context keeps its own meaning rather than being reported
// as a server problem.
func connectError(serverName string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return errdefs.Timeoutf("mcp: server %q: connect timed out: %w", serverName, err)
	}

	if cannotStart(err) {
		return errdefs.Validationf(
			"mcp: server %q: cannot start command: %w", serverName, err)
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errdefs.NotAvailablef(
			"mcp: server %q: cannot start command: %w", serverName, err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return errdefs.NotAvailablef(
			"mcp: server %q: server process exited during startup: %w", serverName, err)
	}
	if isUnreachable(err) {
		return errdefs.NotAvailablef("mcp: server %q: unreachable: %w", serverName, err)
	}
	if isRejection(err) {
		return errdefs.Validationf("mcp: server %q: rejected connection: %w", serverName, err)
	}
	return errdefs.NotAvailablef("mcp: server %q: connect failed: %w", serverName, err)
}

// cannotStart reports whether err is a failure to spawn the child
// process that a retry cannot fix: the command file is missing, is not
// executable, or is not in a format the OS can run — the same three
// cases when the command was a bare name that PATH could not turn into
// a runnable file. A spawn failure outside that set (a fork the kernel
// refused for lack of resources, say) stays retryable.
func cannotStart(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		// A bare command: LookPath failed, either because nothing on
		// PATH has that name or because the match is not executable.
		return errors.Is(err, exec.ErrNotFound) ||
			errors.Is(err, fs.ErrPermission)
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		// A command given as a path: the exec itself failed.
		return errors.Is(pathErr.Err, fs.ErrNotExist) ||
			errors.Is(pathErr.Err, fs.ErrPermission) ||
			errors.Is(pathErr.Err, syscall.ENOEXEC)
	}
	return false
}

// isUnreachable recognises transport-level failures. The go-sdk wraps
// these in its own error types without exported sentinels, so matching
// on the message is the only option available; the categories it feeds
// are coarse enough that a miss degrades to the same NotAvailable
// default rather than mislabelling the failure.
func isUnreachable(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connection refused",
		"no such host",
		"network is unreachable",
		"broken pipe",
		"eof",
		"connection reset",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// isRejection recognises a peer that answered and declined, which is a
// configuration problem on our side rather than an outage on theirs.
func isRejection(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"unsupported protocol version",
		"unauthorized",
		"forbidden",
		"invalid request",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
