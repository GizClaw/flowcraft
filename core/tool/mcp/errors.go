package mcp

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// connectError classifies a go-sdk connect failure into an errdefs
// category so callers can branch on it without string matching.
//
// The distinction that matters operationally is "the server is not
// there" versus "the server is there but rejected us". The first is
// NotAvailable: a missing binary, a refused dial, a child that died
// during startup — retrying later or fixing configuration may fix it,
// and a host attaching several servers should be able to carry on
// without this one. The second is Validation: initialization completed
// far enough for the peer to say no, which points at the request, not
// the environment. A cancelled or expired context keeps its own
// meaning rather than being reported as a server problem.
func connectError(serverName string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return errdefs.Timeoutf("mcp: server %q: connect timed out: %v", serverName, err)
	}

	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errdefs.NotAvailablef(
			"mcp: server %q: cannot start command: %v", serverName, err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return errdefs.NotAvailablef(
			"mcp: server %q: server process exited during startup: %v", serverName, err)
	}
	if isUnreachable(err) {
		return errdefs.NotAvailablef("mcp: server %q: unreachable: %v", serverName, err)
	}
	if isRejection(err) {
		return errdefs.Validationf("mcp: server %q: rejected connection: %v", serverName, err)
	}
	return errdefs.NotAvailablef("mcp: server %q: connect failed: %v", serverName, err)
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
