package graph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

// openDriver opens one session over a compiled graph, which is the only way a caller gets a driver.
func openDriver(t *testing.T, g *Graph, session string, opts ...ProgramOption) (agent.Program, agent.Driver) {
	t.Helper()
	program := NewProgram(g, opts...)
	driver, err := program.Open(agent.SessionInfo{SessionID: session})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return program, driver
}

// turn runs one message through a driver and returns the events it yielded and the first error.
// resume drives one Resume through a driver that supports it, the way the host does.
func resume(t *testing.T, driver agent.Resumer, req agent.Request) ([]agent.Event, error) {
	t.Helper()
	var events []agent.Event
	var first error
	for event, err := range driver.Resume(context.Background(), req) {
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if event != nil {
			events = append(events, event)
		}
	}
	return events, first
}

func turn(t *testing.T, driver agent.Driver, req agent.Request) ([]agent.Event, error) {
	t.Helper()
	var events []agent.Event
	var first error
	for event, err := range driver.Run(context.Background(), req) {
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if event != nil {
			events = append(events, event)
		}
	}
	return events, first
}

// A compiled graph is an engine: the program it becomes satisfies the same contract a remote agent
// does, so a deployment can pick either without special cases.
func TestGraphProgramSatisfiesTheDriverContract(t *testing.T) {
	agenttest.DriverSuite(t, func(t *testing.T) agent.Program {
		g := build(t, `
version: 1
entry: a
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
`, &countKind{})
		return NewProgram(g)
	})
}

// requestFor builds the request the host would: the task to continue, the store, and the identity of
// the deployment writing it.
func requestFor(store agent.Store, taskID, text string) agent.Request {
	return agent.Request{
		Task:        agent.Task{ID: taskID},
		Input:       message.NewTextMessage(message.RoleUser, text),
		Store:       store,
		Fingerprint: "sha256:deployment",
		Binding:     agenttest.BindingFor("conv-1"),
	}
}

// stateOf reads where the task stands from the store, which is where a restarted process finds it.
func stateOf(t *testing.T, store agent.Store, taskID string) agent.TaskState {
	t.Helper()
	checkpoint, err := store.Load(context.Background(), taskID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return checkpoint.Task.State
}

// The timeout bounds the whole run, suspension included: a task that waited past its budget is not
// resumed as if it had just started.
func TestTimeoutCoversTheSuspendedTime(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, `
version: 1
limits: {timeout: 40ms}
entry: ask
state:
  text: {type: text}
nodes:
  - {id: ask, kind: ask, config: {key: text}}
edges:
  - {from: ask, to: end}
output: {key: text}
`, askKind{})

	firstProgram, first := openDriver(t, g, "conv-1")
	defer func() { _ = firstProgram.Close() }()
	defer func() { _ = first.Close() }()
	if _, err := turn(t, first, requestFor(store, "task-1", "hi")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if state := stateOf(t, store, "task-1"); state != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", state)
	}
	time.Sleep(60 * time.Millisecond)

	secondProgram, second := openDriver(t, g, "conv-1")
	defer func() { _ = secondProgram.Close() }()
	defer func() { _ = second.Close() }()
	events, err := resume(t, second.(agent.Resumer), requestFor(store, "task-1", "draft"))
	var canceled bool
	for _, event := range events {
		if status, ok := event.(agent.Status); ok && status.State == agent.StateCanceled {
			canceled = true
		}
	}
	if !canceled {
		t.Error("a restored run past its budget was resumed")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}

// A steer is queued for the next boundary and can be withdrawn until it is applied. The handle is what
// makes that possible: without it a caller that changed its mind could only wait.
func TestSteerQueuesAndWithdraws(t *testing.T) {
	// A frontier that takes long enough to steer inside: the boundary has not arrived yet.
	g := build(t, `
version: 1
input: text
entry: slow
state:
  text: {type: messages, merge: append}
nodes:
  - {id: slow, kind: sleep, config: {d: 300ms}}
  - {id: count, kind: count}
edges:
  - {from: slow, to: count}
  - {from: count, to: end}
`, &sleepKind{}, &countKind{})
	program, driver := openDriver(t, g, "conv-1")
	defer func() { _ = program.Close() }()
	defer func() { _ = driver.Close() }()

	steerer, ok := driver.(agent.Steerer)
	if !ok {
		t.Fatal("the graph driver cannot steer")
	}
	if _, err := steerer.Steer(agent.SteerInput{
		Message: message.NewTextMessage(message.RoleUser, "before a turn"),
	}); !errors.Is(err, agent.ErrSessionIdle) {
		t.Fatalf("Steer with no run = %v, want ErrSessionIdle", err)
	}

	// Start a turn and hold it inside the node, so the boundary has not arrived yet.
	req := requestFor(agenttest.NewMemStore(), "task-1", "hi")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range driver.Run(context.Background(), req) {
		}
	}()
	waitForRun(t, driver)

	id, err := steerer.Steer(agent.SteerInput{
		Message: message.NewTextMessage(message.RoleUser, "hold on"),
		Intent:  agent.SteerRedirect,
	})
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if _, err := steerer.Steer(agent.SteerInput{
		Message: message.NewTextMessage(message.RoleUser, "and also"),
	}); err == nil {
		t.Fatal("a second steer before the boundary was accepted; it would replace the first")
	}
	if err := steerer.Unsteer(id); err != nil {
		t.Fatalf("Unsteer: %v", err)
	}
	if err := steerer.Unsteer(id); !errors.Is(err, agent.ErrSteerUnknown) {
		t.Fatalf("Unsteer of a withdrawn handle = %v, want ErrSteerUnknown", err)
	}
}

// A refusal reaches the caller through the driver too: Steer forwards to the run's gate, so a caller
// that only knows agent.Steerer still learns that its message was never going to be accepted — instead
// of getting a handle for something the boundary will reject or, worse, apply.
func TestDriverSteerRefusesANonUserMessage(t *testing.T) {
	g := build(t, `
version: 1
input: messages
entry: slow
state:
  messages: {type: messages, merge: append}
nodes:
  - {id: slow, kind: sleep, config: {d: 200ms}}
  - {id: count, kind: count}
edges:
  - {from: slow, to: count}
  - {from: count, to: end}
`, &sleepKind{}, &countKind{})
	program, drv := openDriver(t, g, "conv-1")
	defer func() { _ = program.Close() }()
	defer func() { _ = drv.Close() }()

	req := requestFor(agenttest.NewMemStore(), "task-1", "hi")
	go func() {
		for range drv.Run(context.Background(), req) {
		}
	}()
	waitForRun(t, drv)

	steerer := drv.(agent.Steerer)
	if _, err := steerer.Steer(agent.SteerInput{
		Message: message.NewTextMessage(message.RoleAssistant, "not mine to say"),
		Intent:  agent.SteerRedirect,
	}); !errdefs.IsValidation(err) {
		t.Fatalf("Steer = %v, want a validation error", err)
	}
	// A refused input leaves the slot free, so the operator's next attempt works.
	if _, err := steerer.Steer(agent.SteerInput{
		Message: message.NewTextMessage(message.RoleUser, "hold on"),
		Intent:  agent.SteerRedirect,
	}); err != nil {
		t.Fatalf("Steer after a refusal = %v, want queued", err)
	}
}

// waitForRun blocks until the driver holds a run, which is the moment a steer becomes acceptable.
func waitForRun(t *testing.T, drv agent.Driver) {
	t.Helper()
	d, ok := drv.(*driver)
	if !ok {
		t.Fatalf("driver = %T, want the graph's own", drv)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		running := d.run != nil
		d.mu.Unlock()
		if running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the run never started")
}

// failingKind is a node that fails, and says which diagnosis it is.
type failingKind struct{ kind agent.FailureKind }

func (f failingKind) Name() string { return "failing" }

func (f failingKind) FailureKind() agent.FailureKind { return f.kind }

func (f failingKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	return nil, errors.New("the node could not finish")
}

// TestAFailingNodeClassifiesTheTurn is the driver's half of the turn's failure vocabulary: the
// decorator owns timeout, cancellation and panic, and the diagnosis of everything else can only
// come from the layer that made the call. Without it every turn reports "unclassified".
func TestAFailingNodeClassifiesTheTurn(t *testing.T) {
	recorder := telemetrytest.New(t)
	g := build(t, `
version: 1
entry: boom
nodes:
  - {id: boom, kind: failing}
edges:
  - {from: boom, to: end}
`, failingKind{kind: agent.FailureModel})

	program := NewProgram(g)
	defer func() { _ = program.Close() }()
	driver, err := program.Open(agent.SessionInfo{SessionID: "conv-1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = driver.Close() }()

	for _, err := range agent.RunTurn(context.Background(), agent.Request{
		Input:   message.NewTextMessage(message.RoleUser, "hi"),
		Binding: agenttest.BindingFor("conv-1"),
	}, driver) {
		if err != nil {
			break
		}
	}

	span := recorder.RequireNamed(t, "agent.turn")
	if got := telemetrytest.Attrs(span)["error.kind"].String(); got != string(agent.FailureModel) {
		t.Fatalf("error.kind = %q, want %q", got, agent.FailureModel)
	}
}

// A task is one turn: the message after a finished run opens a new run under its own task id, and the
// keys the document marks `carry` are what the conversation keeps. A driver that refused would report
// every second turn as an error, which is what a chat surface would then show the user.
func TestATurnAfterAFinishedTaskStartsANewRun(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, `
version: 1
entry: a
input: messages
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
state:
  messages: {type: messages, merge: append, carry: true}
`, &countKind{})
	driver, err := NewProgram(g).Open(agent.SessionInfo{SessionID: "conv-1", TaskID: "conv-1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = driver.Close() }()

	if _, err := turn(t, driver, requestFor(store, "conv-1", "first")); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := turn(t, driver, requestFor(store, "conv-1/2", "second")); err != nil {
		t.Fatalf("second turn: %v", err)
	}
}
