package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// ExecRequest is a snapshot of one Runner.Exec call (or one
// Runner.Start attempt), shared by predicates (which inspect
// it) and approval callbacks (which present it to the approver). It is
// a DTO rather than loose parameters so new fields remain additive for
// implementors. TTY marks interactive session starts so the approver
// can see that approval covers a persistent command channel rather
// than a single command; it is false for ordinary Exec calls.
type ExecRequest struct {
	Command string
	Args    []string
	Opts    ExecOptions
	TTY     bool
}

// Predicate decides whether an Exec call crosses a policy boundary and
// therefore needs approval. The reason string explains the match to
// the approver (and into audit logs); it is only meaningful when
// matched is true.
//
// Predicates are necessarily heuristic: a decorator sees the command
// and its options, never what the process will actually do at runtime
// ("sh -c" hides everything). They are the tripwire, not the wall —
// OS-level enforcement by the backend remains the wall.
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

// WithApproval returns a Runner that consults approve before
// delegating to inner, but only when the call is out-of-bounds.
// Calls matching allowlist are pre-approved and pass straight through
// without any approver round-trip. When allowlist is non-nil, every
// other call consults the approver (reason "command not in sandbox
// allowlist"); predicates add extra tripwires on top of that (workdir
// escapes, non-default network posture, …). When several predicates
// match, only the first one's reason is reported — one call, one
// decision. With allowlist nil, only predicate-matching calls reach
// the approver.
//
// Contract:
//
//   - allowlist may be nil (no pre-approved commands) or an empty
//     list (every call goes through the approver).
//   - Matching the allowlist bypasses the predicates as well: it is a
//     blanket pre-authorization of the call. Deployments that also
//     want per-call approval on other dimensions (network posture,
//     workdir) should keep those calls out of the allowlist.
//   - Denied decisions surface as errdefs.PolicyDenied and the inner
//     runner is never invoked.
//   - ApprovalFunc errors are fail-closed: the call does not run.
//   - An approved call proceeds with byte-identical ExecOptions; the
//     backend may still reject it on its own grounds (approval does
//     not widen policy, it gates the attempt).
//   - A nil approve with a matching predicate denies the call
//     (fail-closed) rather than panicking.
//
// Composition: place WithDefaults outside WithApproval so defaults are
// merged before this decorator sees the call and the approver receives
// the effective policy. Reversing them makes the approver see only the
// caller's pre-merge options. The recommended local chain uses the
// former ordering.
func WithApproval(inner Runner, approve ApprovalFunc, allowlist *Allowlist, preds ...Predicate) Runner {
	return &approvalRunner{inner: inner, approve: approve, allowlist: allowlist, preds: preds}
}

type approvalRunner struct {
	inner     Runner
	approve   ApprovalFunc
	allowlist *Allowlist
	preds     []Predicate
}

func (r *approvalRunner) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	req := ExecRequest{Command: cmd, Args: args, Opts: opts}
	if err := r.authorize(ctx, req); err != nil {
		return nil, err
	}
	return Exec(ctx, r.inner, cmd, args, opts)
}

// authorize runs the allowlist / predicate / approver chain for one
// request. It is shared by Exec and Runner.Start so interactive
// sessions go through exactly the same fail-closed tripwire.
func (r *approvalRunner) authorize(ctx context.Context, req ExecRequest) error {
	if r.allowlist != nil && r.allowlist.Matches(req) {
		return nil
	}
	for _, p := range r.preds {
		if p == nil {
			return errdefs.PolicyDeniedf(
				"sandbox: nil approval predicate configured (fail-closed)")
		}
		if predicateFunc, ok := p.(PredicateFunc); ok && predicateFunc == nil {
			return errdefs.PolicyDeniedf(
				"sandbox: nil approval predicate function configured (fail-closed)")
		}
		reason, matched := p.Match(cloneExecRequest(req))
		if !matched {
			continue
		}
		return r.requestApproval(ctx, req, reason)
	}
	if r.allowlist != nil {
		return r.requestApproval(ctx, req, "command not in sandbox allowlist")
	}
	return nil
}

func (r *approvalRunner) requestApproval(ctx context.Context, req ExecRequest, reason string) error {
	if r.approve == nil {
		return errdefs.PolicyDeniedf(
			"sandbox: %s (no approver configured)", reason)
	}
	decision, err := r.approve(ctx, ApprovalRequest{
		Exec:   cloneExecRequest(req),
		Reason: reason,
	})
	if err != nil {
		return fmt.Errorf(
			"sandbox: approval for %q failed; not executed (fail-closed): %w", req.Command, err)
	}
	if decision != Allow {
		return errdefs.PolicyDeniedf(
			"sandbox: execution of %q denied: %s", req.Command, reason)
	}
	return nil
}

// Start implements Runner: an interactive session crosses the
// same predicate / approver chain as an Exec call, with TTY surfaced
// on the request so the approver sees it is authorising a persistent
// command channel. Once Start is approved, Read/Write/Resize/Terminate
// never re-enter approval — the policy is fixed at Start.
func (r *approvalRunner) Start(ctx context.Context, spec SessionSpec) (Session, error) {
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("sandbox: SessionSpec.Argv must name a command")
	}
	req := ExecRequest{
		Command: spec.Argv[0],
		Args:    spec.Argv[1:],
		Opts:    spec.Opts,
		TTY:     spec.TTY,
	}
	if err := r.authorize(ctx, req); err != nil {
		return nil, err
	}
	return r.inner.Start(ctx, spec)
}

// List forwards the inner runner.s session list.
func (r *approvalRunner) List(ctx context.Context) ([]SessionInfo, error) {
	return r.inner.List(ctx)
}

// Terminate forwards to the inner runner's session manager.
func (r *approvalRunner) Terminate(ctx context.Context, id string) error {
	return r.inner.Terminate(ctx, id)
}

// cloneExecRequest prevents predicates / approvers from mutating the
// arguments or option maps/slices that are later delegated to the
// backend. Approval observes a snapshot; it is never a policy rewrite
// hook.
func cloneExecRequest(req ExecRequest) ExecRequest {
	out := req
	out.Args = cloneSlice(req.Args)
	out.Opts.Stdin = cloneSlice(req.Opts.Stdin)
	out.Opts.Env.Allow = cloneSlice(req.Opts.Env.Allow)
	if req.Opts.Env.Inject != nil {
		out.Opts.Env.Inject = make(map[string]string, len(req.Opts.Env.Inject))
		for key, value := range req.Opts.Env.Inject {
			out.Opts.Env.Inject[key] = value
		}
	}
	out.Opts.Net.AllowHosts = cloneSlice(req.Opts.Net.AllowHosts)
	return out
}

func cloneSlice[T any](src []T) []T {
	if src == nil {
		return nil
	}
	out := make([]T, len(src))
	copy(out, src)
	return out
}

// Capabilities forwards the inner runner's declaration: approval gates
// calls but does not itself enforce any policy dimension or add a
// session feature.
func (r *approvalRunner) Capabilities() Capabilities {
	return r.inner.Capabilities()
}

// CommandPatterns returns a predicate that matches when the command's
// base name glob-matches any of the patterns (e.g. "rm", "dd",
// "git"). It inspects the command string only — a shell invocation
// hides the real program behind "sh".
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
		if req.Opts.Net.Mode != corenet.NetDefault {
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
		resolved, err := EvalExistingPrefix(abs)
		if err != nil {
			resolved = abs
		}
		if resolved != real && !strings.HasPrefix(resolved, real+string(filepath.Separator)) {
			return fmt.Sprintf("workdir %q resolves outside root %q", wd, real), true
		}
		return "", false
	})
}
