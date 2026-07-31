package sandbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
	sandboxx "github.com/GizClaw/flowcraft/sdkx/sandbox"
)

type recordingRunner struct {
	called bool
	opts   coresandbox.ExecOptions
}

func (r *recordingRunner) Exec(_ context.Context, _ string, _ []string, opts coresandbox.ExecOptions) (*coresandbox.ExecResult, error) {
	r.called = true
	r.opts = opts
	return &coresandbox.ExecResult{}, nil
}

func TestComposeLocal_ApprovalSeesEffectiveDefaults(t *testing.T) {
	inner := &recordingRunner{}
	seenTimeout := time.Duration(0)
	runner := sandboxx.ComposeLocal(inner, sandboxx.LocalPolicy{
		Defaults: coresandbox.ExecOptions{
			Timeout: 3 * time.Second,
			Net:     coresandbox.NetPolicy{Mode: coresandbox.NetDenyAll},
		},
		Approval: func(_ context.Context, req coresandbox.ApprovalRequest) (coresandbox.Decision, error) {
			seenTimeout = req.Exec.Opts.Timeout
			if req.Exec.Opts.Net.Mode != coresandbox.NetDenyAll {
				t.Errorf("approver saw Net mode %v, want effective NetDenyAll", req.Exec.Opts.Net.Mode)
			}
			return coresandbox.Allow, nil
		},
		Predicates: []coresandbox.Predicate{coresandbox.NetNonDefault()},
	})

	if _, err := runner.Exec(context.Background(), "echo", nil, coresandbox.ExecOptions{}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if seenTimeout != 3*time.Second {
		t.Errorf("approver Timeout = %v, want merged 3s", seenTimeout)
	}
	if !inner.called || inner.opts.Timeout != 3*time.Second {
		t.Errorf("inner did not receive effective defaults: called=%v opts=%+v", inner.called, inner.opts)
	}
}

func TestComposeLocal_AllowCommandsRemainsHardGate(t *testing.T) {
	inner := &recordingRunner{}
	runner := sandboxx.ComposeLocal(inner, sandboxx.LocalPolicy{
		AllowedCommands: []string{"echo"},
		Approval: func(context.Context, coresandbox.ApprovalRequest) (coresandbox.Decision, error) {
			return coresandbox.Allow, nil
		},
		Predicates: []coresandbox.Predicate{
			coresandbox.PredicateFunc(func(coresandbox.ExecRequest) (string, bool) {
				return "always", true
			}),
		},
	})

	_, err := runner.Exec(context.Background(), "rm", nil, coresandbox.ExecOptions{})
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("allow-list rejection should be PolicyDenied, got %v", err)
	}
	if inner.called {
		t.Fatal("approval must not bypass the command allow-list")
	}
}

func TestComposeLocal_NilAllowedCommandsOmitsGate(t *testing.T) {
	inner := &recordingRunner{}
	runner := sandboxx.ComposeLocal(inner, sandboxx.LocalPolicy{})

	if _, err := runner.Exec(context.Background(), "anything", nil, coresandbox.ExecOptions{}); err != nil {
		t.Fatalf("nil allow-list should omit the gate: %v", err)
	}
	if !inner.called {
		t.Fatal("call did not reach inner")
	}
}

func TestDefaultLocalPolicy(t *testing.T) {
	root := t.TempDir()
	policy := sandboxx.DefaultLocalPolicy(root, nil, "rm")
	if len(policy.Predicates) != 3 {
		t.Fatalf("Predicates = %d, want workdir + net + command", len(policy.Predicates))
	}

	outside := t.TempDir()
	requests := []coresandbox.ExecRequest{
		{Opts: coresandbox.ExecOptions{WorkDir: outside}},
		{Opts: coresandbox.ExecOptions{
			Net: coresandbox.NetPolicy{Mode: coresandbox.NetDenyAll},
		}},
		{Command: "/bin/rm"},
	}
	for index, req := range requests {
		matched := false
		for _, predicate := range policy.Predicates {
			if _, ok := predicate.Match(req); ok {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("default predicate set did not match request %d: %+v", index, req)
		}
	}
}
