package sandbox_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// scriptedSession replays one canned result through the Session
// interface: [sandbox.Exec] drains the chunks, then Wait reports the
// exit code. Everything sessions do beyond the one-shot flow is
// stubbed out.
type scriptedSession struct {
	result *sandbox.ExecResult
}

func (s *scriptedSession) ID() string { return "scripted" }
func (s *scriptedSession) PID() int   { return 0 }

func (s *scriptedSession) Read(
	_ context.Context, afterSeq int64, _ int,
) (sandbox.SessionOutput, error) {
	if afterSeq > 0 {
		return sandbox.SessionOutput{NextSeq: afterSeq, EOF: true}, nil
	}
	var chunks []sandbox.OutputChunk
	seq := int64(0)
	for _, part := range []struct {
		stream sandbox.SessionStream
		data   string
	}{
		{sandbox.SessionStreamStdout, s.result.Stdout},
		{sandbox.SessionStreamStderr, s.result.Stderr},
	} {
		if part.data == "" {
			continue
		}
		chunks = append(chunks, sandbox.OutputChunk{
			Seq:    seq,
			Stream: part.stream,
			Data:   []byte(part.data),
		})
		seq += int64(len(part.data))
	}
	return sandbox.SessionOutput{NextSeq: seq, Chunks: chunks, EOF: true}, nil
}

func (s *scriptedSession) Write(context.Context, []byte) error { return nil }
func (s *scriptedSession) CloseInput() error                   { return nil }
func (s *scriptedSession) Resize(context.Context, int, int) error {
	return nil
}

func (s *scriptedSession) Signal(
	context.Context, sandbox.SessionSignal,
) error {
	return nil
}

func (s *scriptedSession) Terminate(context.Context) error { return nil }
func (s *scriptedSession) Wait(context.Context) (sandbox.SessionExit, error) {
	return sandbox.SessionExit{Code: s.result.ExitCode}, nil
}

func (s *scriptedSession) Watch(
	context.Context,
) (sandbox.SessionWatcher, error) {
	return nil, errdefs.NotAvailablef("scripted session: no event stream")
}

func (s *scriptedSession) Close() error { return nil }
func (s *scriptedSession) Capabilities() sandbox.SessionCapabilities {
	return sandbox.SessionCapabilities{}
}

// scriptedRunner reports one canned result per Start and records the
// spawn specs, so tests can prove which chain ran and under what
// options.
type scriptedRunner struct {
	result   *sandbox.ExecResult
	startErr error
	specs    []sandbox.SessionSpec
}

func (r *scriptedRunner) Start(
	_ context.Context, spec sandbox.SessionSpec,
) (sandbox.Session, error) {
	r.specs = append(r.specs, spec)
	if r.startErr != nil {
		return nil, r.startErr
	}
	result := r.result
	if result == nil {
		result = &sandbox.ExecResult{}
	}
	return &scriptedSession{result: result}, nil
}

func (r *scriptedRunner) calls() int { return len(r.specs) }

func (r *scriptedRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{}
}

func (r *scriptedRunner) List(context.Context) ([]sandbox.SessionInfo, error) {
	return nil, nil
}

func (r *scriptedRunner) Terminate(context.Context, string) error { return nil }
func (r *scriptedRunner) Close() error                            { return nil }

// fakeEscalator records every prompt and returns one canned decision.
type fakeEscalator struct {
	decision sandbox.EscalationDecision
	err      error
	requests []sandbox.EscalationRequest
}

func (f *fakeEscalator) Escalate(
	_ context.Context, req sandbox.EscalationRequest,
) (sandbox.EscalationDecision, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return sandbox.EscalationDecision{}, f.err
	}
	return f.decision, nil
}

// fakeRules matches spawns against rule strings, exactly like the
// allowlist-backed rule store would.
type fakeRules []string

func (r fakeRules) EscalatedAllowed(req sandbox.ExecRequest) bool {
	got := sandbox.RuleFor(append([]string{req.Command}, req.Args...))
	for _, rule := range r {
		if rule == got {
			return true
		}
	}
	return false
}

// fixedGate is a static EscalationGate.
type fixedGate bool

func (g fixedGate) EscalationAvailable(context.Context) bool { return bool(g) }

// mutatingRules tries to rewrite the request it observes, the way a
// predicate could if it were handed the caller's own slices.
type mutatingRules struct{ seen bool }

func (m *mutatingRules) EscalatedAllowed(req sandbox.ExecRequest) bool {
	m.seen = true
	if len(req.Args) > 0 {
		req.Args[0] = "tampered"
	}
	if len(req.Opts.Stdin) > 0 {
		req.Opts.Stdin[0] = 'X'
	}
	if len(req.Opts.Env.Allow) > 0 {
		req.Opts.Env.Allow[0] = "PATH=/tampered"
	}
	if req.Opts.Env.Inject != nil {
		req.Opts.Env.Inject["FOO"] = "tampered"
	}
	return false
}

// mutatingEscalator rewrites the request it is asked about before
// approving it.
type mutatingEscalator struct{}

func (mutatingEscalator) Escalate(
	_ context.Context, req sandbox.EscalationRequest,
) (sandbox.EscalationDecision, error) {
	if len(req.Exec.Args) > 0 {
		req.Exec.Args[0] = "tampered"
	}
	if len(req.Exec.Opts.Stdin) > 0 {
		req.Exec.Opts.Stdin[0] = 'X'
	}
	return sandbox.EscalationDecision{Allow: true}, nil
}

const pipRefusal = "ERROR: Could not install packages: " +
	"[Errno 1] Operation not permitted: '/Users/x/.local/lib'"

func refusedRunner() *scriptedRunner {
	return &scriptedRunner{result: &sandbox.ExecResult{
		ExitCode: 1,
		Stderr:   pipRefusal + "\n",
	}}
}

func succeededRunner() *scriptedRunner {
	return &scriptedRunner{result: &sandbox.ExecResult{
		ExitCode: 0,
		Stdout:   "installed\n",
	}}
}

func TestDeniedRecognizesSandboxRefusals(t *testing.T) {
	cases := []struct {
		name   string
		result sandbox.ExecResult
		want   bool
	}{
		{
			name: "seatbelt eperm",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "python: " +
				"PermissionError: [Errno 1] Operation not permitted: '/Users/x/.local/lib'"},
			want: true,
		},
		{
			name: "bwrap read-only bind",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "pip: error: " +
				"could not write to '/home/x/.local': Read-only file system"},
			want: true,
		},
		{
			name:   "windows write confinement",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "Access is denied."},
			want:   true,
		},
		{
			name: "powershell dotnet denial",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "Set-Content: " +
				"Access to the path '/home/x/.local/lib' is denied."},
			want: true,
		},
		{
			name: "powershell exception name",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "Exception: " +
				"System.UnauthorizedAccessException: Access is denied"},
			want: true,
		},
		{
			name:   "powershell error category",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "[PermissionDenied]"},
			want:   true,
		},
		{
			// Seatbelt profile syntax is what the backend hands the
			// kernel; a refused write surfaces as the EPERM wording
			// above, never as profile text (see the darwin
			// integration test that pins this against the real
			// backend).
			name: "seatbelt profile text stays quiet",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "deny file-write-create " +
				"/Users/x/.local"},
			want: false,
		},
		{
			name:   "stdout fallback",
			result: sandbox.ExecResult{ExitCode: 1, Stdout: "Operation not permitted"},
			want:   true,
		},
		{
			// A wrapper that moves its own error output must not
			// hide the refusal behind unrelated stderr noise.
			name: "marker on stdout behind stderr",
			result: sandbox.ExecResult{ExitCode: 1,
				Stderr: "note: retrying without the cache\n",
				Stdout: "touch: /Users/x/.local: Operation not permitted\n"},
			want: true,
		},
		{
			name:   "success",
			result: sandbox.ExecResult{ExitCode: 0, Stderr: "Operation not permitted"},
			want:   false,
		},
		{
			name: "command not found",
			result: sandbox.ExecResult{ExitCode: 127, Stderr: "sh: nonexistent: " +
				"Operation not permitted"},
			want: false,
		},
		{
			name: "dash redirection failure",
			result: sandbox.ExecResult{ExitCode: 2, Stderr: "/bin/sh: 1: " +
				"cannot create /var/tmp/x.txt: Read-only file system"},
			want: true,
		},
		{
			name:   "not executable",
			result: sandbox.ExecResult{ExitCode: 126, Stderr: "Operation not permitted"},
			want:   false,
		},
		{
			// ordinary file-mode failures must not prompt
			name: "plain eacces stays quiet",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "cat: /etc/shadow: " +
				"Permission denied"},
			want: false,
		},
		{
			name:   "compile failure",
			result: sandbox.ExecResult{ExitCode: 1, Stderr: "./main.go:12:2: undefined: foo"},
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, got := sandbox.Denied(&tc.result)
			if got != tc.want {
				t.Fatalf("Denied = %v (%q), want %v", got, reason, tc.want)
			}
			if tc.want && reason == "" {
				t.Fatal("denial without a reason")
			}
		})
	}
}

func TestDeniedNilResult(t *testing.T) {
	if _, ok := sandbox.Denied(nil); ok {
		t.Fatal("nil result must not look denied")
	}
}

func TestDenialExcerptPicksDenialLine(t *testing.T) {
	res := &sandbox.ExecResult{
		ExitCode: 1,
		Stderr: "Collecting requests\n" +
			"ERROR: Could not install packages: [Errno 1] Operation not permitted: " +
			"'/Users/x/.local/lib/python3.13/site-packages'\n" +
			"See https://pip.pypa.io for help\n",
	}
	detail := sandbox.DenialExcerpt(res)
	if want := "ERROR: Could not install packages"; !strings.HasPrefix(detail, want) {
		t.Fatalf("detail = %q, want the denial line", detail)
	}
}

func TestDenialExcerptFallsBackAndTruncates(t *testing.T) {
	res := &sandbox.ExecResult{ExitCode: 1, Stderr: "\nfirst failure line\nsecond\n"}
	if got := sandbox.DenialExcerpt(res); got != "first failure line" {
		t.Fatalf("detail = %q, want the first non-empty line", got)
	}
	if got := sandbox.DenialExcerpt(nil); got != "" {
		t.Fatalf("nil detail = %q", got)
	}
	long := strings.Repeat("x", 500)
	got := sandbox.DenialExcerpt(&sandbox.ExecResult{ExitCode: 1, Stderr: long + "\n"})
	if len(got) != 403 || !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated detail = %d bytes, suffix ok = %v",
			len(got), strings.HasSuffix(got, "..."))
	}
	// The cut lands on a rune boundary: 399 ASCII bytes put byte 400 in
	// the middle of the first multi-byte rune, so a byte-wise slice
	// would emit invalid UTF-8.
	wide := strings.Repeat("x", 399) + strings.Repeat("\u00e9", 100)
	got = sandbox.DenialExcerpt(&sandbox.ExecResult{ExitCode: 1, Stderr: wide + "\n"})
	if !utf8.ValidString(got) {
		t.Fatalf("truncated detail = %q, want valid UTF-8", got)
	}
	if want := strings.Repeat("x", 399) + "..."; got != want {
		t.Fatalf("truncated detail = %q, want the cut on the rune boundary", got)
	}
	// A refusal carried on stdout gets the same treatment, even behind
	// unrelated stderr noise.
	got = sandbox.DenialExcerpt(&sandbox.ExecResult{
		ExitCode: 1,
		Stderr:   "note: retrying without the cache\n",
		Stdout:   "touch: /Users/x/.local: Operation not permitted\n",
	})
	if !strings.Contains(got, "Operation not permitted") {
		t.Fatalf("stdout detail = %q, want the denial line", got)
	}
}

func TestRuleForUnwrapsShell(t *testing.T) {
	if got := sandbox.RuleFor([]string{"/bin/sh", "-c", "pip install requests"}); got != "pip install requests" {
		t.Fatalf("rule = %q, want the unwrapped script", got)
	}
	if got := sandbox.RuleFor([]string{"git", "status"}); got != "git status" {
		t.Fatalf("rule = %q", got)
	}
	if got := sandbox.RuleFor(nil); got != "" {
		t.Fatalf("empty argv rule = %q", got)
	}
}

func TestEscalationApprovedRetriesThroughUnconfined(t *testing.T) {
	confined := refusedRunner()
	unconfined := succeededRunner()
	escalator := &fakeEscalator{decision: sandbox.EscalationDecision{Allow: true}}
	esc := &sandbox.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Escalate:   escalator,
		Gate:       fixedGate(true),
	}
	opts := sandbox.ExecOptions{WorkDir: "sub"}

	res, outcome, err := esc.Exec(
		context.Background(), "sh", []string{"-c", "pip install requests"}, opts)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res == nil || res.ExitCode != 0 || res.Stdout != "installed\n" {
		t.Fatalf("result = %+v, want the retry's result", res)
	}
	if outcome != (sandbox.EscalationOutcome{Refused: true, Approved: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
	if confined.calls() != 1 || unconfined.calls() != 1 {
		t.Fatalf("confined calls = %d, unconfined calls = %d, want 1 and 1",
			confined.calls(), unconfined.calls())
	}
	if got := unconfined.specs[0].Opts; !reflect.DeepEqual(got, opts) {
		t.Fatalf("retry opts = %+v, want byte-identical %+v", got, opts)
	}
	if got := strings.Join(unconfined.specs[0].Argv, " "); got != "sh -c pip install requests" {
		t.Fatalf("retry argv = %q", got)
	}
	if len(escalator.requests) != 1 {
		t.Fatalf("asks = %d, want exactly one prompt", len(escalator.requests))
	}
	req := escalator.requests[0]
	if req.Command != "sh -c 'pip install requests'" {
		t.Fatalf("request command = %q", req.Command)
	}
	if req.Rule != "pip install requests" {
		t.Fatalf("request rule = %q, want the unwrapped command", req.Rule)
	}
	if req.Exec.Opts.WorkDir != "sub" || len(req.Exec.Args) != 2 ||
		req.Exec.Args[0] != "-c" {
		t.Fatalf("request exec = %+v, want the refused attempt", req.Exec)
	}
	if !strings.Contains(req.Reason, "operation not permitted") {
		t.Fatalf("request reason = %q", req.Reason)
	}
	if !strings.Contains(req.Detail, "Operation not permitted") {
		t.Fatalf("request detail = %q", req.Detail)
	}
}

func TestEscalationDeniedKeepsConfinedResult(t *testing.T) {
	confined := refusedRunner()
	unconfined := succeededRunner()
	esc := &sandbox.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Escalate:   &fakeEscalator{}, // empty decision: deny
		Gate:       fixedGate(true),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "pip", []string{"install", "requests"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res == nil || res.ExitCode != 1 ||
		!strings.Contains(res.Stderr, "Operation not permitted") {
		t.Fatalf("result = %+v, want the confined refusal", res)
	}
	if unconfined.calls() != 0 {
		t.Fatal("a denied escalation must not run outside the sandbox")
	}
	if outcome != (sandbox.EscalationOutcome{Refused: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationRememberRetriesAndReports(t *testing.T) {
	confined := refusedRunner()
	unconfined := succeededRunner()
	esc := &sandbox.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Escalate: &fakeEscalator{decision: sandbox.EscalationDecision{
			Allow: true, Remember: true,
		}},
		Gate: fixedGate(true),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "pip", []string{"install", "requests"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res == nil || res.ExitCode != 0 {
		t.Fatalf("result = %+v, want the retry's result", res)
	}
	if outcome != (sandbox.EscalationOutcome{
		Refused: true, Approved: true, Remembered: true,
	}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationPromptErrorFailsClosed(t *testing.T) {
	confined := refusedRunner()
	unconfined := succeededRunner()
	esc := &sandbox.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Escalate:   &fakeEscalator{err: errors.New("no UI available")},
		Gate:       fixedGate(true),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "pip", []string{"install", "requests"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("a prompt failure must keep the confined result, got %v", err)
	}
	if res == nil || res.ExitCode != 1 {
		t.Fatalf("result = %+v, want the confined refusal", res)
	}
	if unconfined.calls() != 0 {
		t.Fatal("a failed prompt must not run outside the sandbox")
	}
	if outcome != (sandbox.EscalationOutcome{Refused: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationGateFalseKeepsConfine(t *testing.T) {
	// Read-only sessions: the mode's guarantee is not tradeable, so
	// the user is not even asked.
	escalator := &fakeEscalator{decision: sandbox.EscalationDecision{Allow: true}}
	esc := &sandbox.Escalation{
		Confined:   refusedRunner(),
		Unconfined: succeededRunner(),
		Escalate:   escalator,
		Gate:       fixedGate(false),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "pip", []string{"install", "requests"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res == nil || res.ExitCode != 1 {
		t.Fatalf("result = %+v, want the confined refusal", res)
	}
	if len(escalator.requests) != 0 {
		t.Fatal("gate=false must not prompt")
	}
	if outcome != (sandbox.EscalationOutcome{Refused: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationGateFalseBeatsRememberedRule(t *testing.T) {
	// A remembered rule must not cancel the read-only guarantee
	// either: the confined attempt still runs, and it still stands.
	confined := refusedRunner()
	unconfined := succeededRunner()
	esc := &sandbox.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Rules:      fakeRules{"pip install requests"},
		Gate:       fixedGate(false),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "sh", []string{"-c", "pip install requests"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res == nil || res.ExitCode != 1 {
		t.Fatalf("result = %+v, want the confined refusal", res)
	}
	if confined.calls() != 1 || unconfined.calls() != 0 {
		t.Fatalf("confined calls = %d, unconfined calls = %d, want 1 and 0",
			confined.calls(), unconfined.calls())
	}
	if outcome != (sandbox.EscalationOutcome{Refused: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationRuleSkipsConfinedAttempt(t *testing.T) {
	confined := refusedRunner()
	unconfined := succeededRunner()
	esc := &sandbox.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Rules:      fakeRules{"pip install requests"},
		Gate:       fixedGate(true),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "sh", []string{"-c", "pip install requests"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res == nil || res.ExitCode != 0 {
		t.Fatalf("result = %+v, want the remembered rule's run", res)
	}
	if confined.calls() != 0 {
		t.Fatal("a remembered rule must skip the doomed confined attempt")
	}
	if unconfined.calls() != 1 {
		t.Fatalf("unconfined calls = %d, want 1", unconfined.calls())
	}
	if outcome != (sandbox.EscalationOutcome{Approved: true, Remembered: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationNilEscalatorKeepsFailure(t *testing.T) {
	// Headless deployments have no ask channel: a refusal stays a
	// plain command failure and nothing runs outside.
	unconfined := succeededRunner()
	esc := &sandbox.Escalation{
		Confined:   refusedRunner(),
		Unconfined: unconfined,
		Gate:       fixedGate(true),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "pip", []string{"install", "requests"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res == nil || res.ExitCode != 1 {
		t.Fatalf("result = %+v, want the confined refusal", res)
	}
	if unconfined.calls() != 0 {
		t.Fatal("without an escalator nothing may run outside")
	}
	if outcome != (sandbox.EscalationOutcome{Refused: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationTypedNilGateOffersNothing(t *testing.T) {
	// A typed-nil func boxes into a non-nil interface; the gate must
	// still read as "no" instead of panicking on the call.
	escalator := &fakeEscalator{decision: sandbox.EscalationDecision{Allow: true}}
	unconfined := succeededRunner()
	esc := &sandbox.Escalation{
		Confined:   refusedRunner(),
		Unconfined: unconfined,
		Escalate:   escalator,
		Gate:       sandbox.EscalationGateFunc(nil),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "pip", []string{"install", "requests"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res == nil || res.ExitCode != 1 {
		t.Fatalf("result = %+v, want the confined refusal", res)
	}
	if len(escalator.requests) != 0 || unconfined.calls() != 0 {
		t.Fatal("a nil gate must not offer a retry")
	}
	if outcome != (sandbox.EscalationOutcome{Refused: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationObservesRequestSnapshots(t *testing.T) {
	// The rule store and the escalator observe snapshots, never the
	// caller's own slices: neither can rewrite what runs (the same
	// contract approval predicates have through cloneExecRequest).
	confined := refusedRunner()
	unconfined := succeededRunner()
	rules := &mutatingRules{}
	opts := sandbox.ExecOptions{
		WorkDir: "sub",
		Stdin:   []byte("input"),
		Env: sandbox.EnvPolicy{
			Allow:  []string{"PATH=/usr/bin"},
			Inject: map[string]string{"FOO": "bar"},
		},
	}
	args := []string{"-c", "pip install requests"}
	esc := &sandbox.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Escalate:   mutatingEscalator{},
		Rules:      rules,
		Gate:       fixedGate(true),
	}
	if _, _, err := esc.Exec(context.Background(), "sh", args, opts); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !rules.seen {
		t.Fatal("the rules store must observe the attempt")
	}
	if confined.calls() != 1 || unconfined.calls() != 1 {
		t.Fatalf("calls = %d confined / %d unconfined, want 1 and 1",
			confined.calls(), unconfined.calls())
	}
	wantArgv := []string{"sh", "-c", "pip install requests"}
	for name, runner := range map[string]*scriptedRunner{
		"confined": confined,
		"retry":    unconfined,
	} {
		if got := runner.specs[0].Argv; !slices.Equal(got, wantArgv) {
			t.Fatalf("%s argv = %v, want %v", name, got, wantArgv)
		}
		if got := string(runner.specs[0].Opts.Stdin); got != "input" {
			t.Fatalf("%s stdin = %q", name, got)
		}
		if got := runner.specs[0].Opts.Env; !reflect.DeepEqual(got, opts.Env) {
			t.Fatalf("%s env = %+v, want %+v", name, got, opts.Env)
		}
	}
}

func TestEscalationRetryFailureIsReported(t *testing.T) {
	sentinel := errors.New("unconfined spawn failed")
	unconfined := &scriptedRunner{startErr: sentinel}
	esc := &sandbox.Escalation{
		Confined:   refusedRunner(),
		Unconfined: unconfined,
		Escalate:   &fakeEscalator{decision: sandbox.EscalationDecision{Allow: true}},
		Gate:       fixedGate(true),
	}
	res, outcome, err := esc.Exec(
		context.Background(), "pip", []string{"install", "requests"}, sandbox.ExecOptions{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the retry's failure", err)
	}
	if res != nil {
		t.Fatalf("result = %+v, want none", res)
	}
	if outcome != (sandbox.EscalationOutcome{Refused: true, Approved: true}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestEscalationFalsePositiveStaysDeclinable(t *testing.T) {
	// The detector is a tripwire: a command that legitimately reports
	// "Operation not permitted" trips it too, but nothing runs outside
	// unless the user says so.
	confined := &scriptedRunner{result: &sandbox.ExecResult{
		ExitCode: 1,
		Stderr:   "probe: sysctl: Operation not permitted\n",
	}}
	unconfined := succeededRunner()
	escalator := &fakeEscalator{} // deny
	esc := &sandbox.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Escalate:   escalator,
		Gate:       fixedGate(true),
	}
	res, _, err := esc.Exec(
		context.Background(), "probe", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(escalator.requests) != 1 {
		t.Fatalf("asks = %d, want the declinable prompt", len(escalator.requests))
	}
	if unconfined.calls() != 0 {
		t.Fatal("a declined false positive must not run outside")
	}
	if _, denied := sandbox.Denied(res); !denied {
		t.Fatal("the marker should have tripped the detector")
	}
	if res == nil || res.ExitCode != 1 ||
		!strings.Contains(res.Stderr, "Operation not permitted") {
		t.Fatalf("result = %+v, want the confined result", res)
	}
}

func TestEscalationPassesThroughNonRefusals(t *testing.T) {
	cases := map[string]sandbox.ExecResult{
		"success":         {ExitCode: 0, Stdout: "done\n"},
		"compile failure": {ExitCode: 1, Stderr: "./main.go:12:2: undefined: foo\n"},
		"not executable":  {ExitCode: 126, Stderr: "Operation not permitted\n"},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			confined := &scriptedRunner{result: &want}
			escalator := &fakeEscalator{
				decision: sandbox.EscalationDecision{Allow: true},
			}
			esc := &sandbox.Escalation{
				Confined:   confined,
				Unconfined: succeededRunner(),
				Escalate:   escalator,
				Gate:       fixedGate(true),
			}
			res, outcome, err := esc.Exec(
				context.Background(), "go", []string{"build"}, sandbox.ExecOptions{})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if res.ExitCode != want.ExitCode || res.Stderr != want.Stderr {
				t.Fatalf("result = %+v, want pass-through of %+v", res, want)
			}
			if len(escalator.requests) != 0 {
				t.Fatal("nothing was refused; there must be no prompt")
			}
			if outcome != (sandbox.EscalationOutcome{}) {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestEscalationValidation(t *testing.T) {
	var silent *sandbox.Escalation
	if _, _, err := silent.Exec(
		context.Background(), "ls", nil, sandbox.ExecOptions{},
	); err == nil {
		t.Fatal("nil receiver must fail validation")
	}
	confined := refusedRunner()
	esc := &sandbox.Escalation{Confined: confined, Gate: fixedGate(true)}
	if _, _, err := esc.Exec(
		context.Background(), "ls", nil, sandbox.ExecOptions{},
	); err == nil {
		t.Fatal("a missing unconfined chain must fail validation")
	}
	if confined.calls() != 0 {
		t.Fatal("validation must fail before anything runs")
	}
}

func TestEscalationFirstAttemptErrorPassesThrough(t *testing.T) {
	sentinel := errors.New("confined spawn failed")
	escalator := &fakeEscalator{decision: sandbox.EscalationDecision{Allow: true}}
	esc := &sandbox.Escalation{
		Confined:   &scriptedRunner{startErr: sentinel},
		Unconfined: succeededRunner(),
		Escalate:   escalator,
		Gate:       fixedGate(true),
	}
	_, _, err := esc.Exec(context.Background(), "ls", nil, sandbox.ExecOptions{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the confined start failure", err)
	}
	if len(escalator.requests) != 0 {
		t.Fatal("a start failure is not a refusal")
	}
}
