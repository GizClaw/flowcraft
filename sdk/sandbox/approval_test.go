package sandbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// symlinkDir wraps os.Symlink; the error is tolerated by callers so
// symlink-sensitive cases silently skip on platforms that refuse.
func symlinkDir(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

// spyRunner records calls so approval tests can prove whether the
// inner runner was reached, and with what options.
type spyRunner struct {
	called  bool
	gotCmd  string
	gotArgs []string
	gotOpts sandbox.ExecOptions
}

func (s *spyRunner) Exec(_ context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	s.called = true
	s.gotCmd = cmd
	s.gotArgs = args
	s.gotOpts = opts
	return &sandbox.ExecResult{Stdout: "ran"}, nil
}

func matchAll(reason string) sandbox.Predicate {
	return sandbox.PredicateFunc(func(sandbox.ExecRequest) (string, bool) { return reason, true })
}

func TestWithApproval_NoMatch_PassesThrough(t *testing.T) {
	spy := &spyRunner{}
	asked := false
	r := sandbox.WithApproval(spy,
		func(context.Context, sandbox.ApprovalRequest) (sandbox.Decision, error) {
			asked = true
			return sandbox.Allow, nil
		},
		sandbox.CommandPatterns("rm"),
	)

	res, err := r.Exec(context.Background(), "echo", []string{"hi"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("in-bounds call should run: %v", err)
	}
	if asked {
		t.Fatal("approver must not be consulted for non-matching calls")
	}
	if !spy.called || res.Stdout != "ran" {
		t.Fatal("call should have reached the inner runner")
	}
}

func TestWithApproval_Allow_ExactOptionsForwarded(t *testing.T) {
	spy := &spyRunner{}
	r := sandbox.WithApproval(spy,
		func(_ context.Context, req sandbox.ApprovalRequest) (sandbox.Decision, error) {
			if req.Reason == "" {
				t.Error("approval request must carry the predicate reason")
			}
			if req.Exec.Command != "rm" {
				t.Errorf("Exec.Command = %q, want rm", req.Exec.Command)
			}
			return sandbox.Allow, nil
		},
		sandbox.CommandPatterns("rm"),
	)

	opts := sandbox.ExecOptions{
		WorkDir: "sub",
		Stdin:   []byte("x"),
		Timeout: 42,
		Env:     sandbox.EnvPolicy{Allow: []string{"PATH"}},
	}
	if _, err := r.Exec(context.Background(), "rm", []string{"-f", "a"}, opts); err != nil {
		t.Fatalf("approved call should run: %v", err)
	}
	if !spy.called {
		t.Fatal("approved call must reach inner")
	}
	if spy.gotOpts.Timeout != 42 || spy.gotOpts.WorkDir != "sub" ||
		len(spy.gotOpts.Env.Allow) != 1 || string(spy.gotOpts.Stdin) != "x" {
		t.Errorf("approval must forward ExecOptions unmodified, got %+v", spy.gotOpts)
	}
}

func TestWithApproval_CallbackCannotMutateDelegatedCall(t *testing.T) {
	spy := &spyRunner{}
	r := sandbox.WithApproval(spy,
		func(_ context.Context, req sandbox.ApprovalRequest) (sandbox.Decision, error) {
			req.Exec.Args[0] = "mutated"
			req.Exec.Opts.Stdin[0] = 'X'
			req.Exec.Opts.Env.Allow[0] = "SECRET"
			req.Exec.Opts.Env.Inject["RUN_ID"] = "mutated"
			req.Exec.Opts.Net.AllowHosts[0] = "evil.invalid"
			return sandbox.Allow, nil
		},
		matchAll("synthetic boundary"),
	)
	opts := sandbox.ExecOptions{
		Stdin: []byte("input"),
		Env: sandbox.EnvPolicy{
			Allow:  []string{"PATH"},
			Inject: map[string]string{"RUN_ID": "original"},
		},
		Net: sandbox.NetPolicy{AllowHosts: []string{"example.com"}},
	}
	if _, err := r.Exec(context.Background(), "cmd", []string{"original"}, opts); err != nil {
		t.Fatal(err)
	}
	if spy.gotArgs[0] != "original" || string(spy.gotOpts.Stdin) != "input" ||
		spy.gotOpts.Env.Allow[0] != "PATH" ||
		spy.gotOpts.Env.Inject["RUN_ID"] != "original" ||
		spy.gotOpts.Net.AllowHosts[0] != "example.com" {
		t.Fatalf("approval mutated delegated call: args=%v opts=%+v", spy.gotArgs, spy.gotOpts)
	}
}

func TestWithApproval_Deny_PolicyDenied_InnerNeverCalled(t *testing.T) {
	spy := &spyRunner{}
	r := sandbox.WithApproval(spy,
		func(context.Context, sandbox.ApprovalRequest) (sandbox.Decision, error) {
			return sandbox.Deny, nil
		},
		matchAll("synthetic boundary"),
	)

	_, err := r.Exec(context.Background(), "rm", nil, sandbox.ExecOptions{})
	if err == nil {
		t.Fatal("denied call must fail")
	}
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("denial should be policy-denied, got: %v", err)
	}
	if spy.called {
		t.Fatal("denied call must not reach inner")
	}
}

func TestWithApproval_ApproverError_FailClosed(t *testing.T) {
	spy := &spyRunner{}
	r := sandbox.WithApproval(spy,
		func(context.Context, sandbox.ApprovalRequest) (sandbox.Decision, error) {
			return sandbox.Allow, errors.New("approver UI crashed")
		},
		matchAll("synthetic boundary"),
	)

	_, err := r.Exec(context.Background(), "rm", nil, sandbox.ExecOptions{})
	if err == nil {
		t.Fatal("approver error must fail the call (fail-closed)")
	}
	if spy.called {
		t.Fatal("errored approval must not reach inner")
	}
}

func TestWithApproval_NilApprover_FailClosed(t *testing.T) {
	spy := &spyRunner{}
	r := sandbox.WithApproval(spy, nil, matchAll("synthetic boundary"))

	_, err := r.Exec(context.Background(), "anything", nil, sandbox.ExecOptions{})
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("nil approver should deny, got: %v", err)
	}
	if spy.called {
		t.Fatal("call without approver must not reach inner")
	}
}

func TestWithApproval_NilPredicate_FailClosed(t *testing.T) {
	spy := &spyRunner{}
	r := sandbox.WithApproval(spy, nil, nil)

	_, err := r.Exec(context.Background(), "anything", nil, sandbox.ExecOptions{})
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("nil predicate should deny, got: %v", err)
	}
	if spy.called {
		t.Fatal("call with a nil predicate must not reach inner")
	}
}

func TestWithApproval_FirstMatchOnly(t *testing.T) {
	spy := &spyRunner{}
	asks := 0
	r := sandbox.WithApproval(spy,
		func(context.Context, sandbox.ApprovalRequest) (sandbox.Decision, error) {
			asks++
			return sandbox.Allow, nil
		},
		matchAll("first"), matchAll("second"),
	)

	if _, err := r.Exec(context.Background(), "x", nil, sandbox.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if asks != 1 {
		t.Fatalf("one call must trigger exactly one decision, got %d", asks)
	}
}

func TestCommandPatterns(t *testing.T) {
	p := sandbox.CommandPatterns("rm", "mkfs*")
	for _, tc := range []struct {
		cmd     string
		matched bool
	}{
		{"rm", true},
		{"/bin/rm", true},
		{"mkfs.ext4", true},
		{"cat", false},
		{"rmdir", false}, // glob is on the exact base name, not a prefix
	} {
		_, got := p.Match(sandbox.ExecRequest{Command: tc.cmd})
		if got != tc.matched {
			t.Errorf("Match(%q) = %v, want %v", tc.cmd, got, tc.matched)
		}
	}
}

func TestNetNonDefault(t *testing.T) {
	p := sandbox.NetNonDefault()
	if _, ok := p.Match(sandbox.ExecRequest{}); ok {
		t.Error("NetDefault must not match")
	}
	req := sandbox.ExecRequest{Opts: sandbox.ExecOptions{Net: sandbox.NetPolicy{Mode: sandbox.NetDenyAll}}}
	reason, ok := p.Match(req)
	if !ok || reason == "" {
		t.Error("NetDenyAll must match with a reason")
	}
}

func TestWorkDirOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	p := sandbox.WorkDirOutsideRoot(root)

	cases := []struct {
		name    string
		workDir string
		matched bool
	}{
		{"empty stays in root", "", false},
		{"relative stays in root", "sub/dir", false},
		{"absolute inside root", filepath.Join(root, "sub"), false},
		{"root itself", root, false},
		{"absolute outside", outside, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := sandbox.ExecRequest{Opts: sandbox.ExecOptions{WorkDir: tc.workDir}}
			_, got := p.Match(req)
			if got != tc.matched {
				t.Errorf("Match(WorkDir=%q) = %v, want %v", tc.workDir, got, tc.matched)
			}
		})
	}

	// A symlink pointing outside must be caught after resolution.
	link := filepath.Join(root, "link")
	if err := symlinkDir(outside, link); err == nil {
		req := sandbox.ExecRequest{Opts: sandbox.ExecOptions{WorkDir: link}}
		if _, got := p.Match(req); !got {
			t.Error("symlinked WorkDir escaping root must match")
		}
	}
}

func TestWithApproval_EnforcementForwards(t *testing.T) {
	inner := sandbox.NewLocalRunner(t.TempDir())
	r := sandbox.WithApproval(inner, nil, matchAll("x"))
	got := sandbox.EnforcementOf(r)
	want := sandbox.EnforcementOf(inner)
	if !enforcementEqual(got, want) {
		t.Errorf("Enforcement = %+v, want inner's %+v", got, want)
	}
}

func TestWithApproval_DenyErrorMentionsReason(t *testing.T) {
	spy := &spyRunner{}
	r := sandbox.WithApproval(spy,
		func(context.Context, sandbox.ApprovalRequest) (sandbox.Decision, error) {
			return sandbox.Deny, nil
		},
		matchAll("boundary-X"),
	)
	_, err := r.Exec(context.Background(), "cmd", nil, sandbox.ExecOptions{})
	if err == nil || !strings.Contains(err.Error(), "boundary-X") {
		t.Fatalf("denial error should surface the reason, got: %v", err)
	}
}

func TestInteractivePredicate(t *testing.T) {
	p := sandbox.Interactive()
	if _, matched := p.Match(sandbox.ExecRequest{Command: "sh", TTY: true}); !matched {
		t.Fatal("TTY session start must match Interactive")
	}
	if _, matched := p.Match(sandbox.ExecRequest{Command: "sh"}); matched {
		t.Fatal("plain Exec must not match Interactive")
	}
	if _, matched := p.Match(sandbox.ExecRequest{Command: "sh", TTY: false}); matched {
		t.Fatal("pipe session must not match Interactive")
	}
}
