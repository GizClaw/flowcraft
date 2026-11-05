package sandbox

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/internal/paths"
	"github.com/GizClaw/flowcraft/core/permission"
)

// ExecRequest is a snapshot of one Runner.Exec call (or one
// Runner.Start attempt), shared by predicates (which inspect
// it) and approval callbacks (which present it to the approver). It is
// a DTO rather than loose parameters so new fields remain additive for
// implementors. TTY marks interactive session starts so the approver
// can see that approval covers a persistent command channel rather
// than a single command; it is false for ordinary Exec calls. Stdin
// marks that the child gets an input side (SessionSpec.Stdin — or a TTY,
// whose pty master is writable — for Start; a non-empty
// ExecOptions.Stdin for Exec), so the approver can see that the channel
// carries caller input rather than /dev/null.
type ExecRequest struct {
	Command string
	Args    []string
	Opts    ExecOptions
	TTY     bool
	Stdin   bool
}

// Predicate decides whether an Exec call crosses a policy boundary and
// therefore needs approval. The reason string explains the match to
// the approver (and into audit logs); it is only meaningful when
// matched is true.
//
// Predicates are necessarily heuristic: a decorator sees the command
// and its options, never what the process will actually do at runtime
// (a shell wrapper hides everything). They are the tripwire, not the
// wall — OS-level enforcement by the backend remains the wall.
type Predicate interface {
	Match(req ExecRequest) (reason string, matched bool)
}

// PredicateFunc lets a plain closure act as a Predicate.
type PredicateFunc func(req ExecRequest) (reason string, matched bool)

// Match implements Predicate.
func (f PredicateFunc) Match(req ExecRequest) (string, bool) { return f(req) }

// Decision is the approver's verdict on a boundary-crossing call.
type Decision int

const (
	// Deny blocks the call with an errdefs.PolicyDenied error.
	Deny Decision = iota
	// Allow delegates the call with the exact, unmodified ExecOptions.
	// Approval is never a channel for widening policy.
	Allow
)

// ApprovalRequest is handed to the ApprovalFunc: the call that wants
// to cross a boundary, plus the reason of the first predicate that
// matched.
type ApprovalRequest struct {
	Exec   ExecRequest
	Reason string
}

// ApprovalFunc decides a boundary-crossing call. Returning an error
// means the approval channel itself failed (approver unavailable,
// timeout, UI error); the decorator treats it as fail-closed and never
// executes the command.
type ApprovalFunc func(ctx context.Context, req ApprovalRequest) (Decision, error)

// The approval *decorator* (WithApproval) and its Allowlist ship with the
// implementation layer: the contract keeps the request shape and the
// predicates a host composes, not the wrapper that runs them.

func CommandPatterns(patterns ...string) Predicate {
	return PredicateFunc(func(req ExecRequest) (string, bool) {
		base := filepath.Base(req.Command)
		for _, p := range patterns {
			if ok, _ := filepath.Match(p, base); ok {
				return fmt.Sprintf("command %q matches sensitive pattern %q", base, p), true
			}
		}
		return "", false
	})
}

// Interactive returns a predicate that matches interactive session
// starts (SessionSpec.TTY == true). Ordinary Exec calls never match.
// Because an interactive session is an all-or-nothing command channel,
// deployments that want a human in the loop for persistent shells
// should install this predicate; combined with a nil approver it
// fail-closes into "interactive sessions are forbidden".
func Interactive() Predicate {
	return PredicateFunc(func(req ExecRequest) (string, bool) {
		if req.TTY {
			return "interactive TTY session start", true
		}
		return "", false
	})
}

// NetNonDefault returns a predicate that matches any call requesting a
// network posture other than NetDefault.
func NetNonDefault() Predicate {
	return PredicateFunc(func(req ExecRequest) (string, bool) {
		if req.Opts.Net.Mode != permission.NetDefault {
			return fmt.Sprintf("non-default network policy requested (mode %d)", req.Opts.Net.Mode), true
		}
		return "", false
	})
}

// WorkDirOutsideRoot returns a predicate that matches when an absolute
// WorkDir resolves — symlinks included — outside root. Relative
// WorkDir values always stay under the runner root and never match.
// Note the approval flow this enables: the approver is *asked* about
// the escape; whether the escape then actually runs is still the
// backend's decision (sandbox/local rejects out-of-root WorkDir
// outright).
func WorkDirOutsideRoot(root string) Predicate {
	real, err := filepath.Abs(root)
	if err == nil {
		if resolved, evalErr := filepath.EvalSymlinks(real); evalErr == nil {
			real = resolved
		}
	}
	return PredicateFunc(func(req ExecRequest) (string, bool) {
		wd := req.Opts.WorkDir
		if wd == "" || !filepath.IsAbs(wd) {
			return "", false
		}
		abs := filepath.Clean(wd)
		if !paths.RealWithin(real, abs) {
			return fmt.Sprintf("workdir %q resolves outside root %q", wd, real), true
		}
		return "", false
	})
}
