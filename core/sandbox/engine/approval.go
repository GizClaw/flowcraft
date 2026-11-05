package engine

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

func WithApproval(inner sandbox.Runner, approve sandbox.ApprovalFunc, allowlist *Allowlist, preds ...sandbox.Predicate) sandbox.Runner {
	return &approvalRunner{inner: inner, approve: approve, allowlist: allowlist, preds: preds}
}

type approvalRunner struct {
	inner     sandbox.Runner
	approve   sandbox.ApprovalFunc
	allowlist *Allowlist
	preds     []sandbox.Predicate
}

func (r *approvalRunner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	req := sandbox.ExecRequest{Command: cmd, Args: args, Opts: opts, Stdin: len(opts.Stdin) > 0}
	if err := r.authorize(ctx, req); err != nil {
		return nil, err
	}
	return Exec(ctx, r.inner, cmd, args, opts)
}

// authorize runs the allowlist / predicate / approver chain for one
// request. It is shared by Exec and Runner.Start so interactive
// sessions go through exactly the same fail-closed tripwire.
func (r *approvalRunner) authorize(ctx context.Context, req sandbox.ExecRequest) error {
	if r.allowlist != nil && r.allowlist.Matches(req) {
		return nil
	}
	for _, p := range r.preds {
		if p == nil {
			return errdefs.PolicyDeniedf(
				"sandbox: nil approval predicate configured (fail-closed)")
		}
		if predicateFunc, ok := p.(sandbox.PredicateFunc); ok && predicateFunc == nil {
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

func (r *approvalRunner) requestApproval(ctx context.Context, req sandbox.ExecRequest, reason string) error {
	if r.approve == nil {
		return errdefs.PolicyDeniedf(
			"sandbox: %s (no approver configured)", reason)
	}
	decision, err := r.approve(ctx, sandbox.ApprovalRequest{
		Exec:   cloneExecRequest(req),
		Reason: reason,
	})
	if err != nil {
		return fmt.Errorf(
			"sandbox: approval for %q failed; not executed (fail-closed): %w", req.Command, err)
	}
	if decision != sandbox.Allow {
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
func (r *approvalRunner) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("sandbox: SessionSpec.Argv must name a command")
	}
	req := sandbox.ExecRequest{
		Command: spec.Argv[0],
		Args:    spec.Argv[1:],
		Opts:    spec.Opts,
		TTY:     spec.TTY,
		Stdin:   spec.Stdin || spec.TTY,
	}
	if err := r.authorize(ctx, req); err != nil {
		return nil, err
	}
	return r.inner.Start(ctx, spec)
}

// List forwards the inner runner.s session list.
func (r *approvalRunner) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
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
func cloneExecRequest(req sandbox.ExecRequest) sandbox.ExecRequest {
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
func (r *approvalRunner) Capabilities() sandbox.Capabilities {
	return r.inner.Capabilities()
}

// Close forwards to the inner runner so lifecycle survives decoration.
func (r *approvalRunner) Close() error {
	return r.inner.Close()
}

// OpenJournal forwards to the inner runner: approval gates whether a
// call runs, not what the backend watches afterwards, so the decorated
// runner exposes exactly the inner journal (or NotAvailable when there
// is none).
func (r *approvalRunner) OpenJournal(ctx context.Context) (sandbox.FileJournal, error) {
	return sandbox.OpenJournal(ctx, r.inner)
}
