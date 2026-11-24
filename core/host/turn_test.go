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
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	res, err := Open(t.Context(), deploy.ProcessPlan{Backends: []deploy.PlannedBackend{
		boundBackend("local", backend),
	}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	driver := &seenDriver{log: log}
	agents := NewAgents(res, opts...)
	dep := deploy.Deployment{
		Name:    "referee",
		Grant:   permissiveGrant("local"),
		Trace:   agent.TraceCall,
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &seenProgram{driver: driver}, nil }},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	return agents, driver
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
