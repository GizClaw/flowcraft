package sandbox_test

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

func TestAllowlistRejectsInvalidRules(t *testing.T) {
	for _, rule := range []string{"", "*", "go r*n", "go run * extra", "  "} {
		if _, err := sandbox.NewAllowlist(rule); err == nil {
			t.Fatalf("NewAllowlist(%q) succeeded, want error", rule)
		}
	}
	if _, err := sandbox.NewAllowlist("go *"); err != nil {
		t.Fatalf("NewAllowlist(go *) error = %v", err)
	}
}

func TestAllowlistMatches(t *testing.T) {
	a, err := sandbox.NewAllowlist("go *", "go run *", "git status", "/usr/bin/git *")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  sandbox.ExecRequest
		want bool
	}{
		{"bare go", sandbox.ExecRequest{Command: "go"}, true},
		{"go build", sandbox.ExecRequest{Command: "go", Args: []string{"build", "x"}}, true},
		{"absolute go by basename", sandbox.ExecRequest{Command: "/usr/bin/go", Args: []string{"mod", "tidy"}}, true},
		{"go run", sandbox.ExecRequest{Command: "go", Args: []string{"run"}}, true},
		{"go run main", sandbox.ExecRequest{Command: "go", Args: []string{"run", "main.go"}}, true},
		{"other program", sandbox.ExecRequest{Command: "python3"}, false},
		{"exact git status", sandbox.ExecRequest{Command: "git", Args: []string{"status"}}, true},
		{"git status with args", sandbox.ExecRequest{Command: "git", Args: []string{"status", "-s"}}, false},
		{"bare git", sandbox.ExecRequest{Command: "git"}, false},
		{"slash rule literal", sandbox.ExecRequest{Command: "/usr/bin/git", Args: []string{"log"}}, true},
		{"slash rule not basename", sandbox.ExecRequest{Command: "git", Args: []string{"log"}}, false},
	}
	for _, tc := range cases {
		if got := a.Matches(tc.req); got != tc.want {
			t.Errorf("%s: Matches(%+v) = %v, want %v", tc.name, tc.req, got, tc.want)
		}
	}
}

func TestNormaliseExecUnwrapsShell(t *testing.T) {
	tokens := sandbox.NormaliseExec(sandbox.ExecRequest{
		Command: "sh", Args: []string{"-c", "go run main.go"},
	})
	if want := []string{"go", "run", "main.go"}; !reflect.DeepEqual(tokens, want) {
		t.Fatalf("NormaliseExec(sh -c) = %v, want %v", tokens, want)
	}
	tokens = sandbox.NormaliseExec(sandbox.ExecRequest{
		Command: "git", Args: []string{"status"},
	})
	if want := []string{"git", "status"}; !reflect.DeepEqual(tokens, want) {
		t.Fatalf("NormaliseExec(git status) = %v, want %v", tokens, want)
	}
	tokens = sandbox.NormaliseExec(sandbox.ExecRequest{
		Command: "/bin/sh", Args: []string{"-c", "FOO=1 python3 script.py"},
	})
	if want := []string{"python3", "script.py"}; !reflect.DeepEqual(tokens, want) {
		t.Fatalf("NormaliseExec(env-prefixed) = %v, want %v", tokens, want)
	}
}

func TestAllowlistMatchesShellInvocation(t *testing.T) {
	a, err := sandbox.NewAllowlist("go *", "python3 *", "ls *", "echo *")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  sandbox.ExecRequest
		want bool
	}{
		{"sh -c simple", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "go run main.go"}}, true},
		{"abs sh -c", sandbox.ExecRequest{Command: "/bin/sh", Args: []string{"-c", "python3 script.py"}}, true},
		{"bash -lc", sandbox.ExecRequest{Command: "bash", Args: []string{"-lc", "ls -la"}}, true},
		{"quoted arg", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", `echo 'a b'`}}, true},
		{"env prefix", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "FOO=1 go test ./..."}}, true},
		{"command chain denied", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "npm install && rm -rf /"}}, false},
		{"pipe denied", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "go list | grep x"}}, false},
		{"substitution denied", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "go run $(echo x)"}}, false},
		{"unterminated quote denied", sandbox.ExecRequest{Command: "sh", Args: []string{"-c", `echo "oops`}}, false},
	}
	for _, tc := range cases {
		if got := a.Matches(tc.req); got != tc.want {
			t.Errorf("%s: Matches(%+v) = %v, want %v", tc.name, tc.req, got, tc.want)
		}
	}
}

func TestAllowlistMutationAndUnion(t *testing.T) {
	a, err := sandbox.NewAllowlist("ls *")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Add("go *", "ls *"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ls *", "go *"}; !reflect.DeepEqual(a.Rules(), want) {
		t.Fatalf("Rules() = %v, want %v", a.Rules(), want)
	}

	extra, err := sandbox.NewAllowlist("git status", "go *")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Union(extra); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ls *", "go *", "git status"}; !reflect.DeepEqual(a.Rules(), want) {
		t.Fatalf("Rules() after Union = %v, want %v", a.Rules(), want)
	}

	if err := a.Set([]string{"python3 *"}); err != nil {
		t.Fatal(err)
	}
	if a.Matches(sandbox.ExecRequest{Command: "go"}) {
		t.Fatal("Set did not replace rules: go still matches")
	}
	if !a.Matches(sandbox.ExecRequest{Command: "python3", Args: []string{"x.py"}}) {
		t.Fatal("Set did not apply replacement: python3 does not match")
	}
	if err := a.Add("bad rule * inside"); err == nil {
		t.Fatal("Add applied an invalid rule")
	}
	if want := []string{"python3 *"}; !reflect.DeepEqual(a.Rules(), want) {
		t.Fatalf("Rules() after failed Add = %v, want %v", a.Rules(), want)
	}
}

func TestAllowlistConcurrentAddAndMatch(t *testing.T) {
	a, err := sandbox.NewAllowlist("go *")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = a.Add(fmt.Sprintf("tool-%d *", i))
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if !a.Matches(sandbox.ExecRequest{Command: "go", Args: []string{"build"}}) {
					t.Error("allowlist lost the base rule during concurrent Add")
				}
			}
		}()
	}
	wg.Wait()
	if got := len(a.Rules()); got != 101 {
		t.Fatalf("rules = %d, want 101 (idempotent Add)", got)
	}
}

func TestAllowlistNotAllowedPredicate(t *testing.T) {
	a, err := sandbox.NewAllowlist("go *")
	if err != nil {
		t.Fatal(err)
	}
	p := a.NotAllowed()
	if reason, matched := p.Match(sandbox.ExecRequest{Command: "go"}); matched {
		t.Fatalf("NotAllowed matched an allowlisted command: %s", reason)
	}
	if reason, matched := p.Match(sandbox.ExecRequest{Command: "rm", Args: []string{"-rf", "/"}}); !matched || reason == "" {
		t.Fatalf("NotAllowed did not match an out-of-bounds command: %q, %v", reason, matched)
	}
	var nilList *sandbox.Allowlist
	if _, matched := nilList.NotAllowed().Match(sandbox.ExecRequest{Command: "anything"}); !matched {
		t.Fatal("nil allowlist NotAllowed must match every command (fail closed)")
	}
}

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
