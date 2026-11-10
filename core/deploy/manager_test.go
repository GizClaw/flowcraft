package deploy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

func run(t *testing.T, session *Session) []agent.Event {
	t.Helper()
	var events []agent.Event
	for event, err := range session.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "go")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func TestAddDeploysAnAgent(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "a", "done")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, ok := manager.Session("a", "conv-1")
	if !ok {
		t.Fatal("no handle for a deployed agent")
	}
	run(t, session)
	if state, err := session.State(context.Background()); err != nil || state != agent.StateCompleted {
		t.Fatalf("state = %s (%v), want completed", state, err)
	}
}

// A duplicate name is refused and the deployed agent keeps working.
func TestAddRejectsADuplicateName(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), testHost(t))
	spec := testSpec(t, "a", "done")
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := manager.Add(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Add = %v, want a duplicate complaint", err)
	}
	session, ok := manager.Session("a", "conv-1")
	if !ok {
		t.Fatal("the duplicate add removed the deployed agent")
	}
	run(t, session)
}

func TestAddValidatesTheEngineKind(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), testHost(t))
	cases := map[string]Spec{
		"unregistered": {Name: "a", Engine: EngineSpec{Kind: "missing"}},
		"local without runtime": {
			Name:   "b",
			Engine: EngineSpec{Kind: "done"},
		},
		"remote with a runtime": {
			Name:    "c",
			Engine:  EngineSpec{Kind: "a2a", Settings: rawSettings(map[string]any{"peer": "https://peer.test"})},
			Runtime: config(t),
		},
		"remote without a peer": {Name: "d", Engine: EngineSpec{Kind: "a2a"}},
	}
	for name, spec := range cases {
		if err := manager.Add(context.Background(), spec); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A document is applied whole: one bad spec leaves nothing deployed.
func TestDeployIsAllOrNothing(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), testHost(t))
	if _, err := manager.Deploy(context.Background(),
		testSpec(t, "a", "done"),
		Spec{Name: "x", Engine: EngineSpec{Kind: "not-registered"}},
	); err == nil {
		t.Fatal("a batch with an unregistered engine was accepted")
	}
	if names := manager.Names(); len(names) != 0 {
		t.Fatalf("names = %v after a failed batch, want none", names)
	}
	names, err := manager.Deploy(context.Background(), testSpec(t, "a", "done"), testSpec(t, "b", "done"))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names = %v, want [a b]", names)
	}
}

// Remove stops one agent and leaves the others serving.
func TestRemoveStopsOneAgent(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), testHost(t))
	if _, err := manager.Deploy(context.Background(), testSpec(t, "a", "done"), testSpec(t, "b", "done")); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	first, _ := manager.Session("a", "conv-1")
	second, _ := manager.Session("b", "conv-1")
	if err := manager.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := manager.Session("a", "conv-1"); ok {
		t.Error("the removed agent still has a handle")
	}
	if err := manager.Remove(context.Background(), "a"); err == nil {
		t.Error("removing an undeployed agent succeeded")
	}
	var runErr error
	for _, err := range first.Run(context.Background(), message.NewTextMessage(message.RoleUser, "go")) {
		runErr = err
	}
	if !errors.Is(runErr, agent.ErrClosed) {
		t.Errorf("the removed agent accepted work: %v", runErr)
	}
	run(t, second)
}

// Names is what a host shows an operator: sorted, not map order.
func TestNamesIsSorted(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), testHost(t))
	for _, name := range []string{"zeta", "alpha", "mu"} {
		if err := manager.Add(context.Background(), testSpec(t, name, "done")); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}
	names := manager.Names()
	want := []string{"alpha", "mu", "zeta"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

// Replace ends the old deployment: its digest differs, so its sessions are
// not continued on the new one.
func TestReplaceEndsTheOldDeployment(t *testing.T) {
	store := agenttest.NewMemStore()
	host := testHost(t)
	host.Store = store
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), host)
	if err := manager.Add(context.Background(), testSpec(t, "a", "done")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	first, _ := manager.Session("a", "conv-1")
	run(t, first)
	if state, _ := first.State(context.Background()); state != agent.StateCompleted {
		t.Fatalf("state = %s, want completed", state)
	}
	if err := manager.Replace(context.Background(), testSpec(t, "a", "done")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	// The old checkpoint names another runtime, so the new deployment starts over.
	replacement, ok := manager.Session("a", "conv-1")
	if !ok {
		t.Fatal("no handle after Replace")
	}
	if state, err := replacement.State(context.Background()); err != nil || state != agent.StateSubmitted {
		t.Fatalf("state after Replace = %s (%v), want submitted", state, err)
	}
}

// A drain with no patience reports what is still running instead of pretending.
func TestDrainWaitsAndReportsTimeouts(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("blocking", func() agent.Runner {
		return runner
	})), testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "blocking", "blocking")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, _ := manager.Session("blocking", "conv-1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range session.Run(context.Background(), message.NewTextMessage(message.RoleUser, "go")) {
		}
	}()
	<-runner.started
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Drain(short); err == nil || !strings.Contains(err.Error(), "conv-1") {
		t.Fatalf("Drain with a deadline = %v, want the session in flight", err)
	}
	close(runner.release)
	<-done
}

// Every agent in flight is reported, not just the first one visited.
func TestManagerDrainFansOut(t *testing.T) {
	first := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	second := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	runners := []agent.Runner{first, second}
	var next atomic.Int64
	kinds := BuiltinKinds().WithEngines(EngineKind{
		Name:  "blocking",
		Local: true,
		Build: func(context.Context, EngineDeps) (agent.RunnerFactory, error) {
			return func(context.Context, string, string) (agent.Runner, error) {
				index := (next.Add(1) - 1) % int64(len(runners))
				return runners[index], nil
			}, nil
		},
	})
	manager := newManager(t, kinds, testHost(t))
	if _, err := manager.Deploy(context.Background(), testSpec(t, "a", "blocking"), testSpec(t, "b", "blocking")); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	done := make(chan struct{}, 2)
	for _, name := range []string{"a", "b"} {
		go func(name string) {
			defer func() { done <- struct{}{} }()
			session, ok := manager.Session(name, "conv-"+name)
			if !ok {
				return
			}
			for range session.Run(context.Background(), message.NewTextMessage(message.RoleUser, "go")) {
			}
		}(name)
	}
	<-first.started
	<-second.started
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := manager.Drain(short)
	if err == nil {
		t.Fatal("Drain accepted two runs in flight")
	}
	for _, name := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), name+":") {
			t.Errorf("Drain error %q does not name agent %q", err, name)
		}
	}
	close(first.release)
	close(second.release)
	<-done
	<-done
}

// Close cancels a run, is idempotent, and refuses new work afterwards.
func TestCloseIsIdempotentAndStopsRuns(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("blocking", func() agent.Runner {
		return runner
	})), testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "blocking", "blocking")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, _ := manager.Session("blocking", "conv-1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range session.Run(context.Background(), message.NewTextMessage(message.RoleUser, "go")) {
		}
	}()
	<-runner.started
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-done
	if !runner.cancelled.Load() {
		t.Error("Close did not cancel the run")
	}
	if err := manager.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// OnEvent is how a host streams progress without reaching into the engine; the
// agent name is what lets one host serve many.
func TestOnEventSeesEveryEvent(t *testing.T) {
	var mu sync.Mutex
	var names []string
	host := testHost(t)
	host.OnEvent = func(agentName, _ string, event agent.Event) {
		mu.Lock()
		names = append(names, agentName)
		mu.Unlock()
	}
	manager := newManager(t, BuiltinKinds().WithEngines(engineFor("done", func() agent.Runner {
		return &completedRunner{}
	})), host)
	if err := manager.Add(context.Background(), testSpec(t, "a", "done")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, _ := manager.Session("a", "conv-1")
	events := run(t, session)
	mu.Lock()
	defer mu.Unlock()
	if len(names) == 0 {
		t.Fatal("OnEvent saw nothing")
	}
	if len(names) != len(events) {
		t.Errorf("observed %d of %d events", len(names), len(events))
	}
	if names[0] != "a" {
		t.Errorf("event named agent %q, want a", names[0])
	}
}

func TestHandlerAndSessionForUnknownAgent(t *testing.T) {
	manager := newManager(t, BuiltinKinds(), testHost(t))
	if _, ok := manager.Handler("absent"); ok {
		t.Error("an unknown agent has a handler")
	}
	if _, ok := manager.Session("absent", "conv-1"); ok {
		t.Error("an unknown agent has a session")
	}
}

// The registered kinds are what a host reports at startup.
func TestManagerReportsRegisteredKinds(t *testing.T) {
	manager := newManager(t, BuiltinKinds().WithProtocols(A2AProtocol()), testHost(t))
	engines := manager.EngineNames()
	if len(engines) != 1 || engines[0] != "a2a" {
		t.Errorf("engines = %v, want [a2a]", engines)
	}
	protocols := manager.ProtocolNames()
	if len(protocols) != 1 || protocols[0] != "a2a" {
		t.Errorf("protocols = %v, want [a2a]", protocols)
	}
}
