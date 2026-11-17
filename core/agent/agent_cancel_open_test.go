package agent_test

import (
	"context"
	"errors"
	"iter"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// openCancelRunner drives one run and reports whether the cancellation reached
// it, the way the graph loop does by checking ctx.Err() at the top of every
// iteration.
type openCancelRunner struct {
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	state   agent.TaskState
	seen    bool
	cancels int
}

func newOpenCancelRunner() *openCancelRunner {
	return &openCancelRunner{started: make(chan struct{}), state: agent.StateSubmitted}
}

func (r *openCancelRunner) State() agent.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *openCancelRunner) Run(ctx context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.mu.Lock()
		r.state = agent.StateWorking
		r.mu.Unlock()
		r.once.Do(func() { close(r.started) })
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		<-ctx.Done()
		r.mu.Lock()
		r.seen = true
		r.state = agent.StateCanceled
		r.mu.Unlock()
		yield(agent.Status{State: agent.StateCanceled}, nil)
	}
}

// Cancel is the engine's own stop: with no run in flight it has nothing to
// reach, so it only counts the call that the agent still makes.
func (r *openCancelRunner) Cancel(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels++
	return nil
}

func (r *openCancelRunner) Close() error { return nil }

// sawCancellation reports whether the run observed its context ending.
func (r *openCancelRunner) sawCancellation() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen
}

// runOutcome is where one Run call left the session when its stream ended.
type runOutcome struct {
	state agent.TaskState
	err   error
}

// collectRun drains a run and reports the last state it saw.
func collectRun(a *agent.Agent, sessionID string) runOutcome {
	state := agent.StateSubmitted
	var runErr error
	for event, err := range a.Run(context.Background(), sessionID, message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			runErr = err
			break
		}
		if status, ok := event.(agent.Status); ok {
			state = status.State
		}
	}
	return runOutcome{state: state, err: runErr}
}

// A cancel that arrives while the runner is still being built must stop the run:
// the opener publishes the runner before it wakes the waiter, and a cancel that
// still finds no run in flight latches so the run starts cancelled.
func TestCancelDuringSessionOpenIsHonoured(t *testing.T) {
	opening := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	var built []*openCancelRunner
	factory := func(context.Context, string, string) (agent.Runner, error) {
		runner := newOpenCancelRunner()
		built = append(built, runner)
		once.Do(func() { close(opening) })
		<-hold
		return runner, nil
	}
	deployed, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	// A failed assertion leaves the run goroutine parked on its context; closing
	// the agent is what unblocks it.
	t.Cleanup(func() { _ = deployed.Close() })
	done := make(chan runOutcome, 1)
	go func() { done <- collectRun(deployed, "c1") }()
	<-opening

	cancelled := make(chan error, 1)
	go func() { cancelled <- deployed.Cancel(context.Background(), "c1", "the user left") }()
	// A widener, not a barrier: the cancel is already blocked inside the open,
	// which is still held, so removing this pause only moves the cancel to the
	// window just after it — both have to stop the run.
	time.Sleep(20 * time.Millisecond)
	close(hold)

	if err := <-cancelled; err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("run: %v", got.err)
		}
		if got.state != agent.StateCanceled {
			t.Fatalf("run state = %s, want canceled", got.state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end after a cancel that landed during the open")
	}
	if len(built) != 1 {
		t.Fatalf("runners built = %d, want one", len(built))
	}
	if !built[0].sawCancellation() {
		t.Error("the cancellation never reached the run")
	}
}

// A cancel that lands after the session is open but before the run takes the slot
// has no run to stop; the run that follows still has to observe it.
//
// The reason is looped because it is not part of the contract: Cancel accepts an
// empty one, and a latch keyed on the reason string would silently arm nothing in
// that case.
//
// The fake engine's Cancel is a no-op, which is the case the latch serves: a
// runner with no run in flight and nothing to restore. A graph runner whose
// Cancel marks a paused checkpoint cancelled goes down its own path; this test is
// about the agent layer, not about what one engine does with the call.
func TestCancelBetweenOpenAndRunStart(t *testing.T) {
	for _, reason := range []string{"the user left", ""} {
		t.Run("reason="+strconv.Quote(reason), func(t *testing.T) {
			ctx := context.Background()
			store := agenttest.NewMemStore()
			if _, _, err := store.Save(ctx, agent.Checkpoint{
				Schema: agent.CheckpointSchema,
				Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateInputRequired},
			}, nil); err != nil {
				t.Fatalf("Save: %v", err)
			}
			var built []*openCancelRunner
			factory := func(context.Context, string, string) (agent.Runner, error) {
				runner := newOpenCancelRunner()
				built = append(built, runner)
				return runner, nil
			}
			deployed, err := agent.NewAgent(agent.AgentOptions{Store: store, NewRunner: factory})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			defer func() { _ = deployed.Close() }()
			// The cancel opens the session to reach the paused task and finds no run
			// in flight, which is the window: the slot is still free.
			if err := deployed.Cancel(ctx, "c1", reason); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if len(built) != 1 {
				t.Fatalf("runners built = %d, want the one the cancel opened", len(built))
			}

			done := make(chan runOutcome, 1)
			go func() { done <- collectRun(deployed, "c1") }()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("run: %v", got.err)
				}
				if got.state != agent.StateCanceled {
					t.Fatalf("run state = %s, want canceled: the cancel that landed before the slot was dropped", got.state)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the run did not end after a cancel that landed before it took the slot")
			}
			if !built[0].sawCancellation() {
				t.Error("the cancellation never reached the run")
			}
			if got := built[0].cancels; got != 1 {
				t.Errorf("runner.Cancel calls = %d, want the one best effort stop", got)
			}
		})
	}
}

// refusingRunner reports every cancel as failed and completes its run, so a test
// can tell whether the agent stopped the run anyway.
type refusingRunner struct {
	mu      sync.Mutex
	cancels int
	stopped bool
}

func (r *refusingRunner) State() agent.TaskState { return agent.StateCompleted }

func (r *refusingRunner) Run(ctx context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if err := ctx.Err(); err != nil {
			r.mu.Lock()
			r.stopped = true
			r.mu.Unlock()
			yield(agent.Status{State: agent.StateCanceled}, nil)
			return
		}
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (r *refusingRunner) Cancel(context.Context, string) error {
	r.mu.Lock()
	r.cancels++
	r.mu.Unlock()
	return errors.New("the engine refused the cancel")
}

func (r *refusingRunner) Close() error { return nil }

// A cancel the engine refuses is reported as a failure, and it must not silently
// stop the run that follows: the caller was told the stop did not land.
func TestRefusedCancelDoesNotStopTheNextRun(t *testing.T) {
	ctx := context.Background()
	store := agenttest.NewMemStore()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: "t1", SessionID: "c1", State: agent.StateInputRequired},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	runner := &refusingRunner{}
	factory := func(context.Context, string, string) (agent.Runner, error) { return runner, nil }
	deployed, err := agent.NewAgent(agent.AgentOptions{Store: store, NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer func() { _ = deployed.Close() }()

	if err := deployed.Cancel(ctx, "c1", "the user left"); err == nil {
		t.Fatal("Cancel reported success for an engine that refused the stop")
	}
	got := collectRun(deployed, "c1")
	if got.err != nil {
		t.Fatalf("run: %v", got.err)
	}
	if got.state != agent.StateCompleted {
		t.Fatalf("run state = %s, want completed: the refused cancel was applied behind the caller's back", got.state)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.stopped {
		t.Error("the run started cancelled after a cancel the engine refused")
	}
}
