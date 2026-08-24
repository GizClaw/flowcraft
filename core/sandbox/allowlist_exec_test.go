//go:build unix

package sandbox_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// TestWithApprovalUsesAllowlist exercises the approval/allowlist
// decorators through real local-runner executions, so it is unix-only
// (the local backend cannot start sessions elsewhere).
func TestWithApprovalUsesAllowlist(t *testing.T) {
	inner := localRunner(t)
	var approvals atomic.Int64
	approve := sandbox.ApprovalFunc(func(context.Context, sandbox.ApprovalRequest) (sandbox.Decision, error) {
		approvals.Add(1)
		return sandbox.Allow, nil
	})
	allowlist, err := sandbox.NewAllowlist("echo *")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// In-bounds call: allowlist pre-approves, approver is never asked.
	runner := sandbox.WithApproval(inner, approve, allowlist)
	result, err := sandbox.Exec(ctx, runner, "echo", []string{"hi"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec(echo hi): %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "hi\n" {
		t.Fatalf("result = %+v", result)
	}
	if approvals.Load() != 0 {
		t.Fatalf("approver called %d times for an allowlisted command", approvals.Load())
	}

	// sh -c unwrapping happens before allowlist matching.
	result, err = sandbox.Exec(ctx, runner, "sh", []string{"-c", "echo hi"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec(sh -c echo hi): %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "hi\n" {
		t.Fatalf("result = %+v", result)
	}
	if approvals.Load() != 0 {
		t.Fatalf("approver called for an unwrapped allowlisted command")
	}

	// Out-of-bounds with a nil approver fails closed without executing.
	denyRunner := sandbox.WithApproval(inner, nil, allowlist)
	if _, err := sandbox.Exec(ctx, denyRunner, "ls", nil, sandbox.ExecOptions{}); !errdefs.IsPolicyDenied(err) {
		t.Fatalf("Exec(ls) error = %v, want policy denied", err)
	}
	if approvals.Load() != 0 {
		t.Fatalf("nil approver was invoked")
	}

	// Out-of-bounds with an approving approver executes and asks once.
	runner = sandbox.WithApproval(inner, approve, allowlist)
	result, err = sandbox.Exec(ctx, runner, "ls", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec(ls): %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if approvals.Load() != 1 {
		t.Fatalf("approver calls = %d, want 1", approvals.Load())
	}
}
