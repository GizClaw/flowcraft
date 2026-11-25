package host

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

// seenDriver records the request and the identity it ran under, so a test can assert what the host
// assembled rather than only that a turn finished.
type seenDriver struct {
	log        *journal
	requests   []agent.Request
	identities []identity.Run
}

func (d *seenDriver) Run(ctx context.Context, req agent.Request) iter.Seq2[agent.Event, error] {
	run, _ := identity.Lookup(ctx)
	d.requests = append(d.requests, req)
	d.identities = append(d.identities, run)
	return func(yield func(agent.Event, error) bool) {
		yield(agent.Text{Text: "ok"}, nil)
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (d *seenDriver) Close() error { return nil }

type seenProgram struct{ driver *seenDriver }

func (p *seenProgram) Open(agent.SessionInfo) (agent.Driver, error) { return p.driver, nil }
func (p *seenProgram) Close() error                                 { return nil }

// seenHost opens one session whose driver the test can inspect.
func seenHost(t *testing.T, opts ...AgentsOption) (*Agents, *seenDriver) {
	t.Helper()
	driver := &seenDriver{log: &journal{}}
	agents := hostFor(t, func() (agent.Program, error) { return &seenProgram{driver: driver}, nil }, opts...)
	return agents, driver
}

// hostFor opens one session under a program the test supplies, which is how a case drives a
// specific behaviour through the whole host path: process, deployment, session and turn.
func hostFor(t testing.TB, program func() (agent.Program, error), opts ...AgentsOption) *Agents {
	t.Helper()
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	res, err := Open(t.Context(), deploy.ProcessPlan{
		Backends: []deploy.PlannedBackend{
			boundBackend("local", backend),
		},
		Observability: deploy.Observability{Trace: telemetry.TraceCall},
	}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	agents := NewAgents(res, opts...)
	dep := deploy.Deployment{
		Name:    "referee",
		Program: deploy.PlannedProgram{Impl: "graph", New: program},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	return agents
}

// TestTheTurnCarriesTheSessionIdentity is the identity row the framework promises: every span below a
// turn can say which session and which turn it belongs to.
func TestTheTurnCarriesTheSessionIdentity(t *testing.T) {
	agents, driver := seenHost(t, WithIdentity("acme", "authority-1"))

	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	if len(driver.identities) != 1 {
		t.Fatalf("turns = %d", len(driver.identities))
	}
	run := driver.identities[0]
	if run.Tenant != "acme" || run.Agent != "referee" || run.SessionID != "s1" {
		t.Fatalf("identity = %+v, want the session's", run)
	}
	if run.Authority != "authority-1" || run.Bind == "" {
		t.Fatalf("identity = %+v, want the authority and the grant digest", run)
	}
	if run.Ordinal != 1 || run.TaskID != "s1" {
		t.Fatalf("turn facts = ordinal %d task %q", run.Ordinal, run.TaskID)
	}
}

// TestASecondTurnCountsUp keeps the ordinal meaningful: it is what tells two turns of one session apart
// in a trace.
func TestASecondTurnCountsUp(t *testing.T) {
	agents, driver := seenHost(t)
	for range 2 {
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}
	if len(driver.identities) != 2 {
		t.Fatalf("turns = %d", len(driver.identities))
	}
	if driver.identities[0].Ordinal != 1 || driver.identities[1].Ordinal != 2 {
		t.Fatalf("ordinals = %d,%d", driver.identities[0].Ordinal, driver.identities[1].Ordinal)
	}
}

// TestTheStoreFeedsTheTaskAndTheFingerprint is the half of the request assembly the reviewer found
// untested: a resumed task, the fingerprint, and a store failure that must abort before any event.
func TestTheStoreFeedsTheTaskAndTheFingerprint(t *testing.T) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Fingerprint: "fp-1",
		Task:        agent.Task{ID: "s1", SessionID: "s1", State: agent.StateInputRequired},
	}, []agent.EventRecord{{Kind: "message", Payload: []byte(`"earlier"`)}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	agents, driver := seenHost(t, WithStore(store), WithFingerprint("fp-1"))
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	req := driver.requests[0]
	if req.Task.State != agent.StateWorking {
		t.Fatalf("task state = %q, want the resumed task put back to working", req.Task.State)
	}
	if req.Store == nil || req.Fingerprint != "fp-1" {
		t.Fatalf("request = %+v, want the store and the fingerprint", req)
	}
}

// TestAFinishedTaskIsNotResumed is the other half of that pair: a task is one turn, so the turn after a
// finished one is a new task — and the conversation crosses the boundary through the keys a document
// marks `carry`, not through one endless run.
func TestAFinishedTaskIsNotResumed(t *testing.T) {
	store := agenttest.NewMemStore()
	if _, _, err := store.Save(context.Background(), agent.Checkpoint{
		Fingerprint: "fp-1",
		Task:        agent.Task{ID: "s1", SessionID: "s1", State: agent.StateCompleted},
	}, []agent.EventRecord{{Kind: "message", Payload: []byte(`"earlier"`)}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	agents, driver := seenHost(t, WithStore(store), WithFingerprint("fp-1"))
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "again")) {
	}
	req := driver.requests[0]
	if req.Task.ID == "s1" || !strings.HasPrefix(req.Task.ID, "s1/") {
		t.Fatalf("task id = %q, want a new task for the new turn", req.Task.ID)
	}
	if req.Task.SessionID != "s1" {
		t.Fatalf("session id = %q, want the conversation it belongs to", req.Task.SessionID)
	}
}

// failingStore is a store whose reads fail, which must stop the turn before it starts.
type failingStore struct{}

func (failingStore) Save(context.Context, agent.Checkpoint, []agent.EventRecord, ...agent.SaveOption) (int64, int64, error) {
	return 0, 0, nil
}

func (failingStore) Load(context.Context, string) (agent.Checkpoint, error) {
	return agent.Checkpoint{}, errors.New("database down")
}

func (failingStore) LatestForSession(context.Context, string) (agent.Checkpoint, error) {
	return agent.Checkpoint{}, errors.New("database down")
}

func (failingStore) Events(context.Context, string) ([]agent.EventRecord, error) {
	return nil, errors.New("database down")
}

func (failingStore) EventsAfter(context.Context, string, int64) ([]agent.EventRecord, error) {
	return nil, errors.New("database down")
}

func (failingStore) SetServerVersion(context.Context, string, int64) error {
	return errors.New("database down")
}

func (failingStore) TruncateEvents(context.Context, string, int64, ...string) error {
	return errors.New("database down")
}

func (failingStore) List(context.Context) ([]agent.TaskSummary, error) {
	return nil, errors.New("database down")
}

func (failingStore) Delete(context.Context, string) error { return errors.New("database down") }

func (failingStore) PutBlob(context.Context, []byte) (string, error) {
	return "", errors.New("database down")
}

func (failingStore) GetBlob(context.Context, string) ([]byte, error) {
	return nil, errors.New("database down")
}

// TestAStoreFailureStopsTheTurnBeforeItStarts: a session whose history cannot be read must not run
// against a task nobody loaded.
func TestAStoreFailureStopsTheTurnBeforeItStarts(t *testing.T) {
	agents, driver := seenHost(t, WithStore(failingStore{}))

	events := 0
	var failure error
	for _, err := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			failure = err
		}
		events++
	}
	if failure == nil || !strings.Contains(failure.Error(), "database down") {
		t.Fatalf("err = %v, want the store's", failure)
	}
	if len(driver.requests) != 0 {
		t.Fatal("the driver ran for a turn that could not load its task")
	}
}

// TestTheRunSpanCarriesTheSessionsIdentity is the span the plan says this layer owns.
func TestTheRunSpanCarriesTheSessionsIdentity(t *testing.T) {
	recorder := telemetrytest.New(t)
	agents, _ := seenHost(t, WithIdentity("acme", "authority-1"))

	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	span := recorder.RequireNamed(t, "host.run")
	attrs := telemetrytest.Attrs(span)
	for key, want := range map[string]string{
		"session.id":  "s1",
		"agent.name":  "referee",
		"tenant.id":   "acme",
		"agent.state": "completed",
	} {
		if got := attrs[key].String(); !strings.Contains(got, want) {
			t.Fatalf("%s = %q, want %q; attrs: %v", key, got, want, attrs)
		}
	}
}

// failingDriver records a classification and then fails, which is what a driver does with the
// box: it is the layer that knows whether the model or a tool was what died.
type failingDriver struct{ kind agent.FailureKind }

func (d failingDriver) Run(ctx context.Context, req agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		agent.RecordFailure(ctx, d.kind)
		yield(nil, errors.New("the call did not finish"))
	}
}

func (d failingDriver) Close() error { return nil }

type classifiedProgram struct{ kind agent.FailureKind }

func (p classifiedProgram) Open(agent.SessionInfo) (agent.Driver, error) {
	return failingDriver(p), nil
}

func (p classifiedProgram) Close() error { return nil }

// TestBothSpansOfATurnReadOneClassification is the turn's two clocks sharing one verdict. The
// box is installed before the run span opens, so the driver's classification reaches host.run
// and agent.turn alike: a reader who filters on either name sees the same answer.
func TestBothSpansOfATurnReadOneClassification(t *testing.T) {
	recorder := telemetrytest.New(t)
	agents := hostFor(t, func() (agent.Program, error) {
		return classifiedProgram{kind: agent.FailureModel}, nil
	})

	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}

	for _, name := range []string{"host.run", "agent.turn"} {
		span := recorder.RequireNamed(t, name)
		attrs := telemetrytest.Attrs(span)
		if got := attrs["error.kind"].String(); got != string(agent.FailureModel) {
			t.Fatalf("%s: error.kind = %q, want %q", name, got, agent.FailureModel)
		}
	}
}

// TestTheRunSinkIsWiredFromTheHostOption is the seam a host uses to feed a dataset, a billing line or
// an evaluation corpus: the option reaches every turn, and the record it receives is the one the turn
// decorator built from the same fold the span and the metrics read.
func TestTheRunSinkIsWiredFromTheHostOption(t *testing.T) {
	var got []agent.RunRecord
	agents, _ := seenHost(t, WithIdentity("acme", "authority-1"),
		WithRunSink(func(record agent.RunRecord) { got = append(got, record) }))

	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}

	if len(got) != 1 {
		t.Fatalf("run records = %d, want 1", len(got))
	}
	record := got[0]
	if record.Agent != "referee" || record.SessionID != "s1" || record.TaskID != "s1" {
		t.Fatalf("record identity = %+v", record)
	}
	if record.TenantID != "acme" {
		t.Fatalf("tenant = %q, want the host's", record.TenantID)
	}
	if record.State != agent.StateCompleted {
		t.Fatalf("state = %q, want completed", record.State)
	}
}

// TestTaskIDsSurviveAReopen is the durability half of "a task is one turn": the id of a new task
// must not repeat one the store already holds, or the new turn overwrites the old row and
// interleaves two event logs. A per-session counter restarts when the conversation is reopened,
// which is exactly the case this pins.
func TestTaskIDsSurviveAReopen(t *testing.T) {
	store := agenttest.NewMemStore()
	agents, driver := seenHost(t, WithStore(store), WithFingerprint("fp-1"))

	// Two turns: the first mints the task, the second (after a terminal one) mints a new id.
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "one")) {
	}
	if err := agents.CloseSession(t.Context(), "s1"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	// The same conversation id reopened: a fresh Session, whose ordinal starts over.
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "two")) {
	}

	seen := map[string]bool{}
	for _, request := range driver.requests {
		if seen[request.Task.ID] {
			t.Fatalf("task id %q was minted twice across a reopen", request.Task.ID)
		}
		seen[request.Task.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("task ids = %v, want two distinct ones", seen)
	}
}
