package agent_test

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// fakeRunner is a scripted engine: one Run entry that starts a Task, or
// continues one that is waiting for input — the runner's own state decides.
type fakeRunner struct {
	mu          sync.Mutex
	state       agent.TaskState
	pause       bool
	block       bool
	runCalls    int
	cancelCalls int
	closeCalls  int
	received    []message.Message
	contextSeen string
}

type contextKey struct{}

func newFakeFactory(pause bool) (func(context.Context, string, string) (agent.Runner, error), *[]*fakeRunner) {
	var mu sync.Mutex
	built := make([]*fakeRunner, 0, 4)
	factory := func(context.Context, string, string) (agent.Runner, error) {
		runner := &fakeRunner{state: agent.StateSubmitted, pause: pause}
		mu.Lock()
		built = append(built, runner)
		mu.Unlock()
		return runner, nil
	}
	return factory, &built
}

// closes reports how many times the runner was closed. It takes the lock because
// a close can happen on the sweeper's goroutine, not the test's.
func (f *fakeRunner) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls
}

// calls reports how many runs the fake has started.
func (f *fakeRunner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCalls
}

func (f *fakeRunner) State() agent.TaskState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeRunner) Run(ctx context.Context, msg message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if value, ok := ctx.Value(contextKey{}).(string); ok {
			f.mu.Lock()
			f.contextSeen = value
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.runCalls++
		f.received = append(f.received, msg)
		paused := f.state == agent.StateInputRequired
		finished := f.state.Terminal()
		f.state = agent.StateWorking
		f.mu.Unlock()
		if finished {
			// A finished task reports its state and does nothing else, like a
			// graph run that has already ended.
			f.mu.Lock()
			f.state = agent.StateCompleted
			f.mu.Unlock()
			yield(agent.Status{State: agent.StateCompleted}, nil)
			return
		}
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		if f.block {
			<-ctx.Done()
			f.mu.Lock()
			f.state = agent.StateCanceled
			f.mu.Unlock()
			yield(agent.Status{State: agent.StateCanceled}, nil)
			return
		}
		if paused {
			// An answer completes the Task; there is no second entry point.
			f.mu.Lock()
			f.state = agent.StateCompleted
			f.mu.Unlock()
			yield(agent.Status{State: agent.StateCompleted}, nil)
			return
		}
		if !yield(agent.Text{Text: "answer"}, nil) {
			return
		}
		f.mu.Lock()
		end := agent.StateCompleted
		if f.pause {
			end = agent.StateInputRequired
		}
		f.state = end
		f.mu.Unlock()
		yield(agent.Status{State: end}, nil)
	}
}

func (f *fakeRunner) Cancel(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
	if !f.state.Terminal() {
		f.state = agent.StateCanceled
	}
	return nil
}

func (f *fakeRunner) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

func drain(t *testing.T, sequence iter.Seq2[agent.Event, error]) []agent.Event {
	t.Helper()
	events := make([]agent.Event, 0, 8)
	for event, err := range sequence {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func firstError(sequence iter.Seq2[agent.Event, error]) error {
	for _, err := range sequence {
		return err
	}
	return nil
}

// One message is one run, and the sequence carries it.
func TestAgentRunsOneMessage(t *testing.T) {
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	sequence := a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi"))
	if got := len(*built); got != 0 {
		t.Fatalf("a runner was built before the sequence was consumed")
	}
	events := drain(t, sequence)
	if len(events) != 3 {
		t.Fatalf("events = %d, want working, text, completed", len(events))
	}
	if text, ok := events[1].(agent.Text); !ok || text.Text != "answer" {
		t.Errorf("second event = %#v, want the answer text", events[1])
	}
	if state, _ := a.State(context.Background(), "c1"); state != agent.StateCompleted {
		t.Errorf("state = %s, want completed", state)
	}
	if runner := (*built)[0]; runner.runCalls != 1 {
		t.Errorf("run = %d, want one", runner.runCalls)
	}
}

// A message to a paused Task is the answer: the same entry continues it, and
// the runner's own state decides how.
func TestAgentContinuesAPausedTaskThroughTheSameEntry(t *testing.T) {
	factory, built := newFakeFactory(true)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	drain(t, a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "ask")))
	if state, _ := a.State(context.Background(), "c1"); state != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", state)
	}
	events := drain(t, a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "answer")))
	if len(events) != 2 {
		t.Fatalf("events = %d, want working then completed", len(events))
	}
	if status, ok := events[1].(agent.Status); !ok || status.State != agent.StateCompleted {
		t.Errorf("last event = %#v, want completed", events[1])
	}
	runner := (*built)[0]
	if runner.runCalls != 2 {
		t.Errorf("run = %d, want the paused task continued through the same entry", runner.runCalls)
	}
	if state, _ := a.State(context.Background(), "c1"); state != agent.StateCompleted {
		t.Errorf("state = %s, want completed", state)
	}
}

// A second run on a busy session is refused, not queued.
func TestAgentRefusesASecondRun(t *testing.T) {
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event, err := range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "first")) {
			if err != nil {
				return
			}
			if _, ok := event.(agent.Text); ok {
				close(started)
				<-release
			}
		}
	}()
	<-started
	err = firstError(a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "second")))
	if !errors.Is(err, agent.ErrSessionBusy) {
		t.Fatalf("second run error = %v, want ErrSessionBusy", err)
	}
	close(release)
	<-done
}

// A checkpoint from another deployment is refused when the task is addressed
// explicitly.
func TestAgentRefusesAnotherDeploymentsCheckpoint(t *testing.T) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:one",
		Task: agent.Task{
			ID:        "t1",
			SessionID: "c1",
			State:     agent.StateWorking,
		},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{
		Store:     store,
		Identity:  "sha256:two",
		NewRunner: factory,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	err = firstError(a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi"), agent.WithTaskID("t1")))
	if !errors.Is(err, agent.ErrDeploymentChanged) {
		t.Fatalf("error = %v, want ErrDeploymentChanged", err)
	}
}

// A stored task that paused is adopted and answered, with the task id the store
// remembers.
func TestAgentAdoptsAStoredPausedTask(t *testing.T) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:one",
		Task: agent.Task{
			ID:        "t1",
			SessionID: "c1",
			State:     agent.StateInputRequired,
		},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var taskIDs []string
	factory := func(_ context.Context, _, taskID string) (agent.Runner, error) {
		taskIDs = append(taskIDs, taskID)
		return &fakeRunner{state: agent.StateInputRequired}, nil
	}
	a, err := agent.NewAgent(agent.AgentOptions{
		Store:     store,
		Identity:  "sha256:one",
		NewRunner: factory,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "answer")))
	if len(taskIDs) != 1 || taskIDs[0] != "t1" {
		t.Fatalf("runner built for %v, want the stored task t1", taskIDs)
	}
}

// MaxSessions evicts the oldest idle session and closes its runner.
func TestAgentEvictsIdleSessions(t *testing.T) {
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory, MaxSessions: 1})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "one")))
	drain(t, a.Run(ctx, "c2", message.NewTextMessage(message.RoleUser, "two")))
	if state, _ := a.State(ctx, "c1"); state != agent.StateSubmitted {
		t.Errorf("c1 state = %s after eviction, want submitted", state)
	}
	if len(*built) != 2 || (*built)[0].closeCalls != 1 {
		t.Errorf("evicted runner was not closed: %+v", *built)
	}
	if state, _ := a.State(ctx, "c2"); state != agent.StateCompleted {
		t.Errorf("c2 state = %s, want completed", state)
	}
}

// A session without an id is refused before anything is built.
func TestAgentRejectsAnEmptySession(t *testing.T) {
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := firstError(a.Run(context.Background(), "", message.NewTextMessage(message.RoleUser, "hi"))); !errors.Is(err, agent.ErrSessionRequired) {
		t.Fatalf("error = %v, want ErrSessionRequired", err)
	}
	if len(*built) != 0 {
		t.Errorf("a runner was built for an empty session")
	}
}

// A finished task takes no message: the next one starts a new task in the same
// session, with its own identity and runner.
func TestAgentStartsTheNextTaskAfterCompletion(t *testing.T) {
	var taskIDs []string
	var mu sync.Mutex
	built := make([]*fakeRunner, 0, 4)
	factory := func(_ context.Context, _, taskID string) (agent.Runner, error) {
		mu.Lock()
		taskIDs = append(taskIDs, taskID)
		runner := &fakeRunner{state: agent.StateSubmitted}
		built = append(built, runner)
		mu.Unlock()
		return runner, nil
	}
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "one")))
	events := drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "two")))
	if len(events) != 3 {
		t.Fatalf("second task events = %d, want the run itself", len(events))
	}
	if len(built) != 2 {
		t.Fatalf("runners built = %d, want one per task", len(built))
	}
	if taskIDs[0] == taskIDs[1] {
		t.Errorf("both tasks share the identity %q", taskIDs[0])
	}
	if built[0].closeCalls != 1 {
		t.Errorf("the finished task's runner was not closed")
	}
}

// A task that stopped mid-run is not open for messages: it has to be cancelled
// first, because the step may have had side effects.
func TestAgentRefusesATaskThatStoppedMidRun(t *testing.T) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateWorking},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{Store: store, NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	err = firstError(a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi")))
	if !errors.Is(err, agent.ErrTaskInProgress) {
		t.Fatalf("error = %v, want ErrTaskInProgress", err)
	}
}

// A message addressed to a finished task is refused instead of silently
// starting something else under that id.
func TestAgentRefusesAFinishedTaskAddressedExplicitly(t *testing.T) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateCompleted},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{Store: store, NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	err = firstError(a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi"), agent.WithTaskID("t1")))
	if !errors.Is(err, agent.ErrTaskFinished) {
		t.Fatalf("error = %v, want ErrTaskFinished", err)
	}
}

// Cancel stops a live run through its context; the runner's own Cancel must not
// run concurrently with it.
func TestAgentCancelsARunThroughItsContext(t *testing.T) {
	var runner *fakeRunner
	factory := func(context.Context, string, string) (agent.Runner, error) {
		runner = &fakeRunner{state: agent.StateSubmitted, block: true}
		return runner, nil
	}
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	started := make(chan struct{})
	done := make(chan agent.TaskState, 1)
	go func() {
		state := agent.StateWorking
		for event, err := range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "go")) {
			if err != nil {
				break
			}
			if status, ok := event.(agent.Status); ok {
				state = status.State
				if status.State == agent.StateWorking {
					close(started)
				}
			}
		}
		done <- state
	}()
	<-started
	if err := a.Cancel(context.Background(), "c1", "client cancelled"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if state := <-done; state != agent.StateCanceled {
		t.Fatalf("state = %s, want canceled", state)
	}
	if runner.cancelCalls != 0 {
		t.Errorf("runner.Cancel ran while the runner was driving")
	}
}

// A paused task from a previous process can still be cancelled.
func TestAgentCancelsAStoredPausedTask(t *testing.T) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateInputRequired},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	factory, built := newFakeFactory(true)
	a, err := agent.NewAgent(agent.AgentOptions{Store: store, NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := a.Cancel(ctx, "c1", "no longer needed"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(*built) != 1 || (*built)[0].cancelCalls != 1 {
		t.Fatalf("the stored task was not cancelled: %+v", *built)
	}
}

// The host decides what a run executes under, and the agent passes it through.
func TestAgentAppliesTheRunContext(t *testing.T) {
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: factory,
		RunContext: func(ctx context.Context, _ string) context.Context {
			return context.WithValue(ctx, contextKey{}, "tool-view")
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	drain(t, a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")))
	if got := (*built)[0].contextSeen; got != "tool-view" {
		t.Errorf("runner saw %q, want the host's run context", got)
	}
}

// Every event crosses the observer, so a host can stream without the store.
func TestAgentObservesEveryEvent(t *testing.T) {
	factory, _ := newFakeFactory(false)
	var seen int
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: factory,
		OnEvent:   func(string, agent.Event) { seen++ },
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	events := drain(t, a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")))
	if seen != len(events) {
		t.Errorf("observed %d of %d events", seen, len(events))
	}
}

// Close stops a run in flight and releases the runner.
func TestAgentCloseStopsARun(t *testing.T) {
	var runner *fakeRunner
	factory := func(context.Context, string, string) (agent.Runner, error) {
		runner = &fakeRunner{state: agent.StateSubmitted, block: true}
		return runner, nil
	}
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	started := make(chan struct{})
	done := make(chan agent.TaskState, 1)
	go func() {
		state := agent.StateWorking
		for event, err := range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "go")) {
			if err != nil {
				break
			}
			if status, ok := event.(agent.Status); ok {
				state = status.State
				if status.State == agent.StateWorking {
					close(started)
				}
			}
		}
		done <- state
	}()
	<-started
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if state := <-done; state != agent.StateCanceled {
		t.Fatalf("state = %s, want canceled after Close", state)
	}
	if runner.closeCalls != 1 {
		t.Errorf("runner closed %d times, want 1", runner.closeCalls)
	}
	if err := firstError(a.Run(context.Background(), "c2", message.NewTextMessage(message.RoleUser, "hi"))); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("run after Close = %v, want ErrClosed", err)
	}
}

// Two callers arriving on a cold session build one runner, not two: the
// second waits for the open instead of racing it.
func TestAgentOpensASessionOnceUnderConcurrency(t *testing.T) {
	var mu sync.Mutex
	var built int
	gate := make(chan struct{})
	factory := func(context.Context, string, string) (agent.Runner, error) {
		mu.Lock()
		built++
		mu.Unlock()
		<-gate
		return &fakeRunner{state: agent.StateSubmitted, block: true}, nil
	}
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	type outcome struct {
		err    error
		events int
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	run := func() {
		<-start
		events := 0
		var runErr error
		for event, err := range a.Run(context.Background(), "c1", message.NewTextMessage(message.RoleUser, "hi")) {
			if err != nil {
				runErr = err
				break
			}
			if event != nil {
				events++
			}
		}
		results <- outcome{err: runErr, events: events}
	}
	go run()
	go run()
	close(start)
	time.Sleep(50 * time.Millisecond)
	close(gate)
	// The winner holds the slot until it is cancelled; the loser reports busy
	// without ever building a second runner.
	time.Sleep(100 * time.Millisecond)
	if err := a.Cancel(context.Background(), "c1", "test over"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	first, second := <-results, <-results
	if built != 1 {
		t.Fatalf("runners built = %d, want one", built)
	}
	busy := 0
	for _, got := range []outcome{first, second} {
		if errors.Is(got.err, agent.ErrSessionBusy) {
			busy++
		}
	}
	if busy != 1 {
		t.Fatalf("busy outcomes = %d, want exactly one: %+v %+v", busy, first, second)
	}
}

// Opening one session does not block the table: a lookup elsewhere answers
// while the runner is still being built.
func TestAgentDoesNotBlockOnASlowOpen(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	factory := func(context.Context, string, string) (agent.Runner, error) {
		once.Do(func() { close(entered) })
		<-gate
		return &fakeRunner{state: agent.StateSubmitted}, nil
	}
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		drain(t, a.Run(context.Background(), "slow", message.NewTextMessage(message.RoleUser, "hi")))
	}()
	<-entered
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		if _, err := a.State(context.Background(), "other"); err != nil {
			t.Errorf("State: %v", err)
		}
	}()
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("State blocked while another session was being opened")
	}
	close(gate)
	<-done
}

// A session from another deployment is closed where it stands instead of
// being adopted again on every open.
func TestAgentClosesATaskFromAnotherDeployment(t *testing.T) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:one",
		Task:        agent.Task{ID: "t1", SessionID: "c1", State: agent.StateInputRequired},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{
		Store:     store,
		Identity:  "sha256:two",
		NewRunner: factory,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi")))
	if len(*built) != 1 {
		t.Fatalf("runners built = %d, want the new task's", len(*built))
	}
	checkpoint, err := store.Load(ctx, "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if checkpoint.Task.State != agent.StateCanceled {
		t.Errorf("abandoned task state = %s, want canceled", checkpoint.Task.State)
	}
}

// OnEvict marks a session leaving the table, not a task rolling over: the
// second is the same session.
func TestOnEvictFiresForEvictionNotRollover(t *testing.T) {
	var mu sync.Mutex
	var evicted []string
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner:   factory,
		MaxSessions: 1,
		OnEvict: func(sessionID string) {
			mu.Lock()
			evicted = append(evicted, sessionID)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "one")))
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "two")))
	mu.Lock()
	rolled := len(evicted)
	mu.Unlock()
	if rolled != 0 {
		t.Fatalf("rollover fired OnEvict: %v", evicted)
	}
	// A second session pushes the first out of the bounded table.
	drain(t, a.Run(ctx, "c2", message.NewTextMessage(message.RoleUser, "three")))
	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != "c1" {
		t.Fatalf("evicted = %v, want the first session", evicted)
	}
}
