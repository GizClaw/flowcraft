package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// gate is a backend that records every command it is asked to start, so a test can assert whether
// the approval rule let one through.
type gate struct {
	log     *journal
	started [][]string
}

func (g *gate) Close() error { return nil }

func (g *gate) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Policy: sandbox.Enforcement{FilesystemBounds: true}}
}

func (g *gate) Start(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	g.started = append(g.started, spec.Argv)
	return nil, errors.New("session body not used in these tests")
}

func (g *gate) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }

func (g *gate) Terminate(context.Context, string) error { return nil }

// reviewer records what it was asked and answers with a fixed verdict.
type reviewer struct {
	asked []ApprovalRequest
	err   error
}

func (r *reviewer) Approve(_ context.Context, request ApprovalRequest) error {
	r.asked = append(r.asked, request)
	return r.err
}

// approvalHost opens one session with an approval policy installed, and hands back the process
// capability that policy decorated.
func approvalHost(t *testing.T, approval Approval) (sandbox.SessionStarter, *gate) {
	t.Helper()
	log := &journal{}
	backend := &gate{log: log}
	res, err := Open(t.Context(), deploy.ProcessPlan{Backends: []deploy.PlannedBackend{
		boundBackend("local", &boundary{log: log, filesystemBounds: true}),
	}}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	// The resource holds the boundary; the gate is the starter the session's expectation is about.
	res.backends["local"] = func(sandbox.Confine) (sandbox.Runner, error) { return backend, nil }

	agents := NewAgents(res, WithApproval(approval))
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, &boundary{log: log})
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	starter, ok := platform.ProcessSlot.Get(session.Binding().Caps)
	if !ok || starter == nil {
		t.Fatal("the session has no process capability")
	}
	return starter, backend
}

func spec(argv ...string) sandbox.SessionSpec {
	return sandbox.SessionSpec{ID: "cmd", Argv: argv, Opts: sandbox.ExecOptions{WorkDir: pathtest.Abs("srv")}}
}

// TestAWhitelistedCommandRunsWithoutAsking is the ordinary case: the commands an operator already
// decided about do not interrupt every turn.
func TestAWhitelistedCommandRunsWithoutAsking(t *testing.T) {
	reviewer := &reviewer{}
	starter, backend := approvalHost(t, Approval{Approver: reviewer, Whitelist: Whitelist{{"git", "status"}}})

	_ = start(t, starter, spec("git", "status", "--short"))
	if len(reviewer.asked) != 0 {
		t.Fatalf("the approver was asked about a whitelisted command: %+v", reviewer.asked)
	}
	if len(backend.started) != 1 {
		t.Fatal("the command never reached the backend")
	}
}

// TestTheApproverDecidesEverythingElse covers both verdicts, and the request the approver sees.
func TestTheApproverDecidesEverythingElse(t *testing.T) {
	reviewer := &reviewer{err: errors.New("not this one")}
	starter, backend := approvalHost(t, Approval{Approver: reviewer, Whitelist: Whitelist{{"git", "status"}}})

	_ = start(t, starter, spec("rm", "-rf", pathtest.Abs("srv")))
	if len(backend.started) != 0 {
		t.Fatal("a refused command reached the backend")
	}
	if len(reviewer.asked) != 1 {
		t.Fatalf("asked = %d, want one", len(reviewer.asked))
	}
	got := reviewer.asked[0]
	if got.Agent != "referee" || got.SessionID != "s1" {
		t.Fatalf("request = %+v, want the session's identity", got)
	}
	if strings.Join(got.Argv, " ") != "rm -rf "+pathtest.Abs("srv") || got.WorkDir != pathtest.Abs("srv") {
		t.Fatalf("request = %+v, want the command as written", got)
	}
}

// TestAnApprovedCommandRuns is the other verdict: a nil from the approver is the only thing that lets
// the command through.
func TestAnApprovedCommandRuns(t *testing.T) {
	reviewer := &reviewer{}
	starter, backend := approvalHost(t, Approval{Approver: reviewer})
	if err := start(t, starter, spec("ls")); err == nil {
		t.Fatal("the backend error was swallowed")
	}
	if len(backend.started) != 1 {
		t.Fatal("the approved command never reached the backend")
	}
}

// TestARefusalIsAPolicyDenial keeps the classification: a host rule that refuses is a policy denial
// with the approval check, which is what a model reads to correct itself and what a dashboard counts.
func TestARefusalIsAPolicyDenial(t *testing.T) {
	reviewer := &reviewer{err: &policy.Denial{Check: policy.CheckApproval, Reason: "operator said no"}}
	starter, _ := approvalHost(t, Approval{Approver: reviewer})

	err := start(t, starter, spec("rm", "-rf", pathtest.Roots()))
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("err = %v, want a policy denial", err)
	}
	denial, ok := policy.AsDenial(err)
	if !ok || denial.Check != policy.CheckApproval {
		t.Fatalf("denial = %+v, want the approval check", denial)
	}
	if !strings.Contains(denial.Reason, "operator said no") {
		t.Fatalf("reason = %q, want the approver's", denial.Reason)
	}
}

// TestAFailedApproverKeepsItsDetailOutOfThePrompt is the leak this pins shut: the reason is rendered
// back to the model, and an approver that failed on an HTTP call or a file read puts paths, endpoints
// or token fragments in its error text. The model gets a fixed sentence; the caller and the log get
// the cause.
func TestAFailedApproverKeepsItsDetailOutOfThePrompt(t *testing.T) {
	const leaked = "vault: 403 for token sk-live-abcdef"
	reviewer := &reviewer{err: errors.New(leaked)}
	starter, backend := approvalHost(t, Approval{Approver: reviewer})

	err := start(t, starter, spec("ls"))
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("err = %v, want a policy denial", err)
	}
	denial, ok := policy.AsDenial(err)
	if !ok {
		t.Fatalf("err = %v, want a *policy.Denial", err)
	}
	if strings.Contains(denial.Reason, "sk-live") || strings.Contains(denial.Error(), "sk-live") {
		t.Fatalf("the approver's text reached the model-facing denial: %q", denial.Error())
	}
	if denial.Cause == nil || !strings.Contains(denial.Cause.Error(), "sk-live") {
		t.Fatalf("cause = %v, want the approver's failure preserved for the caller", denial.Cause)
	}
	if len(backend.started) != 0 {
		t.Fatal("a command ran after the approver failed")
	}
}

// TestAPolicyWithNeitherRuleRefusesEverything is the fail-closed direction: "not decided yet" must not
// mean "allowed".
func TestAPolicyWithNeitherRuleRefusesEverything(t *testing.T) {
	starter, backend := approvalHost(t, Approval{})
	if err := start(t, starter, spec("ls")); !errdefs.IsPolicyDenied(err) {
		t.Fatalf("err = %v, want a policy denial", err)
	}
	if len(backend.started) != 0 {
		t.Fatal("an unapproved command reached the backend")
	}
}

// TestNoPolicyInstallsTheBackendUndecorated keeps approval opt-in: a process that never configured it
// is not silently refusing every command.
func TestNoPolicyInstallsTheBackendUndecorated(t *testing.T) {
	log := &journal{}
	g := &gate{log: log}
	res, err := Open(t.Context(), deploy.ProcessPlan{Backends: []deploy.PlannedBackend{
		boundBackend("local", &boundary{log: log, filesystemBounds: true}),
	}}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	res.backends["local"] = func(sandbox.Confine) (sandbox.Runner, error) { return g, nil }

	agents := NewAgents(res)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, &boundary{log: log})
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	starter, ok := platform.ProcessSlot.Get(session.Binding().Caps)
	if !ok {
		t.Fatal("the session has no process capability")
	}
	_ = start(t, starter, spec("ls"))
	if len(g.started) != 1 {
		t.Fatal("an undecorated process refused a command")
	}
}

// TestWhitelistCovers pins the matching rule, including the two directions a prefix gets wrong.
func TestWhitelistCovers(t *testing.T) {
	list := Whitelist{{"git", "status"}, {"ls"}}
	for name, tc := range map[string]struct {
		argv []string
		want bool
	}{
		"exact":          {[]string{"ls"}, true},
		"with arguments": {[]string{"ls", "-la"}, true},
		"prefix":         {[]string{"git", "status", "-s"}, true},
		"different verb": {[]string{"git", "push"}, false},
		"shorter":        {[]string{"git"}, false},
		"unrelated":      {[]string{"rm", "-rf", pathtest.Roots()}, false},
		"empty argv":     {nil, false},
	} {
		if got := list.Covers(tc.argv); got != tc.want {
			t.Fatalf("%s: Covers = %v, want %v", name, got, tc.want)
		}
	}
	if (Whitelist{{}}).Covers([]string{"anything"}) {
		t.Fatal("an empty entry matched everything")
	}
}

// start runs one command through the capability and fails the test only on no error at all when the
// backend is expected to be reached (it returns a sentinel).
func start(t *testing.T, starter sandbox.SessionStarter, spec sandbox.SessionSpec) error {
	t.Helper()
	_, err := starter.Start(t.Context(), spec)
	return err
}

var _ agent.Driver = (*sessionDriver)(nil)
