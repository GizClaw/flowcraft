package host

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"go.opentelemetry.io/otel/codes"
)

// identityDriver records the identity each turn ran under, which is what a span would render.
type identityDriver struct {
	runs  []identity.Run
	state agent.TaskState
}

func (d *identityDriver) Run(ctx context.Context, _ agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		run, _ := identity.Lookup(ctx)
		d.runs = append(d.runs, run)
		state := d.state
		if state == "" {
			state = agent.StateCompleted
		}
		yield(agent.Status{State: state}, nil)
	}
}

func (d *identityDriver) Close() error { return nil }

type identityProgram struct{ driver *identityDriver }

func (p *identityProgram) Open(agent.SessionInfo) (agent.Driver, error) { return p.driver, nil }
func (p *identityProgram) Close() error                                 { return nil }

// delegatedHost installs a parent that may delegate to a child, over one process.
func delegatedHost(t *testing.T, mode policy.DelegationMode) (*Agents, *identityDriver) {
	t.Helper()
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)

	parentGrant := permissiveGrant("local")
	parentGrant.Delegation = &policy.Delegation{
		Targets: []string{"child"},
		Modes:   []policy.DelegationMode{mode},
	}
	childDriver := &identityDriver{}
	parent, _ := sessionDeployment(t, "parent", parentGrant, log, backend)
	child := deploy.Deployment{
		Name:    "child",
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &identityProgram{driver: childDriver}, nil }},
	}
	if err := agents.Add(parent, child); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "p1", "parent", parentGrant); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	return agents, childDriver
}

// TestADelegatedChildRunsUnderItsOwnIdentity is the audit half of delegation: the child's spans
// must name the child's conversation and the child's bind, not the parent's. The parent's identity
// was on the context, the injection refused the child's, and the error was swallowed — so every
// child span reported the parent's session and a grant the child never had.
func TestADelegatedChildRunsUnderItsOwnIdentity(t *testing.T) {
	agents, childDriver := delegatedHost(t, policy.DelegationSync)
	parent, _ := agents.Session("p1")

	outcome, err := delegatorOf(agents, parent).Delegate(t.Context(), platform.DelegationRequest{
		Target: "child", Mode: "sync", Input: "please review",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if len(childDriver.runs) != 1 {
		t.Fatalf("child turns = %d, want 1", len(childDriver.runs))
	}
	run := childDriver.runs[0]
	if run.SessionID != outcome.Session {
		t.Fatalf("child session = %q, want %q", run.SessionID, outcome.Session)
	}
	if run.Agent != "child" {
		t.Fatalf("child agent = %q, want the child deployment", run.Agent)
	}
	if run.ChainID == "" || run.Depth != 1 {
		t.Fatalf("chain = %q depth %d, want the first edge's lineage", run.ChainID, run.Depth)
	}
	if run.TaskID == "" || run.Ordinal != 1 {
		t.Fatalf("turn facts = task %q ordinal %d", run.TaskID, run.Ordinal)
	}
}

// TestASuccessfulTurnHasNoFailedRunSpan is the observation nobody noticed: the failure box is
// installed before the run span opens, so "a box exists" was read as "a failure was classified" and
// every clean turn's run span said Error — with a warning per turn.
func TestASuccessfulTurnHasNoFailedRunSpan(t *testing.T) {
	recorder := telemetrytest.New(t)
	agents, _ := delegatedHost(t, policy.DelegationSync)
	defer recorder.Reset()

	for range agents.Run(t.Context(), "p1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	span := recorder.RequireNamed(t, "host.run")
	if span.Status().Code != codes.Ok {
		t.Fatalf("run span status = %v, want Ok", span.Status().Code)
	}
	if _, failed := telemetrytest.Attrs(span)[telemetry.AttrErrorKind]; failed {
		t.Fatal("a successful run carries an error kind")
	}
}

// TestOnSessionRunsAtOpenAndCanRefuse is the hook that was documented, chained and never called: a
// host policy that prepares a conversation got silence, and one that refused it was ignored.
func TestOnSessionRunsAtOpenAndCanRefuse(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)

	var seen []agent.SessionInfo
	spy := agent.Hooks{OnSession: func(_ context.Context, info agent.SessionInfo) error {
		seen = append(seen, info)
		return nil
	}}
	refusing := agent.Hooks{OnSession: func(context.Context, agent.SessionInfo) error {
		return errors.New("memory is unavailable")
	}}
	dep := deploy.Deployment{
		Name:    "referee",
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &identityProgram{driver: &identityDriver{}}, nil }},
		Hooks:   []deploy.PlannedHook{{Impl: "spy", New: func() (agent.Hooks, error) { return spy, nil }}},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(seen) != 1 || seen[0].SessionID != "s1" || seen[0].Agent != "referee" {
		t.Fatalf("OnSession saw %+v, want the session that was opened", seen)
	}
	if seen[0].State != agent.StateSubmitted || seen[0].Resumed {
		t.Fatalf("OnSession state = %q resumed=%v, want a new conversation", seen[0].State, seen[0].Resumed)
	}

	// The same hook, refusing: the open must fail rather than run a turn blind.
	refuseDep := dep
	refuseDep.Name = "halting"
	refuseDep.Hooks = []deploy.PlannedHook{{Impl: "refusing", New: func() (agent.Hooks, error) { return refusing, nil }}}
	if err := agents.Add(refuseDep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s2", "halting", permissiveGrant("local")); err == nil {
		t.Fatal("a refusing OnSession did not refuse the open")
	}
}
