package agenttest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// ProgramFactory builds a fresh program for each subtest.
type ProgramFactory func(t *testing.T) agent.Program

// DriverSuite runs the contract every agent.Program (and the drivers it opens) should pass.
//
// It is deliberately about the contract and not about behaviour: what a driver does with a
// message is its own business, and a suite that asserted content would only fit the driver
// it was written next to.
func DriverSuite(t *testing.T, build ProgramFactory) {
	t.Helper()
	t.Run("RunEndsWithATerminalStatus", func(t *testing.T) { driverTerminalStatus(t, build) })
	t.Run("RunStopsOnContextCancel", func(t *testing.T) { driverStopsOnCancel(t, build) })
	t.Run("CloseIsIdempotent", func(t *testing.T) { driverCloseIdempotent(t, build) })
	t.Run("OpenIsCheapAndRepeatable", func(t *testing.T) { programOpenRepeatable(t, build) })
	t.Run("OpenIsSafeForConcurrentUse", func(t *testing.T) { programConcurrentOpen(t, build) })
	t.Run("ResumeAndPausedAreDeclaredTogether", func(t *testing.T) { resumePairing(t, build) })
}

// resumePairing keeps the two halves of "the run can wait" in one driver.
//
// [agent.StateInputRequired] is a promise that a caller can continue the run, and [agent.Resumer] is
// the only way to keep it. A driver that implements one without the other leaves its callers holding
// a state they cannot leave — either a paused run nobody can resume, or a resume verb on a driver
// that never pauses.
func resumePairing(t *testing.T, build ProgramFactory) {
	t.Helper()
	driver := open(t, build(t), "resume-pairing")
	_, resumer := driver.(agent.Resumer)
	_, paused := driver.(agent.Paused)
	if resumer != paused {
		t.Fatalf("Resumer implemented = %v, Paused implemented = %v; they are one capability", resumer, paused)
	}
}

// open is the one way the suite gets a driver: through the program, so a program that
// cannot open a session fails here rather than in a document test.
func open(t *testing.T, program agent.Program, session string) agent.Driver {
	t.Helper()
	driver, err := program.Open(agent.SessionInfo{SessionID: session})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if driver == nil {
		t.Fatal("Open returned a nil driver")
	}
	return driver
}

// driverTerminalStatus pins the one thing a host relies on to know a turn finished: the
// stream ends in a terminal state, not in silence.
func driverTerminalStatus(t *testing.T, build ProgramFactory) {
	t.Helper()
	program := build(t)
	defer func() { _ = program.Close() }()
	driver := open(t, program, "s1")
	defer func() { _ = driver.Close() }()

	state, ok := drainTurn(t, driver)
	if !ok {
		t.Fatal("the driver produced no terminal status")
	}
	if !state.Terminal() {
		t.Fatalf("terminal state = %q, which is not terminal", state)
	}
}

// driverStopsOnCancel pins that a cancelled context ends the stream: a driver that keeps
// stepping after cancellation makes a cancelled turn indistinguishable from a hung one.
func driverStopsOnCancel(t *testing.T, build ProgramFactory) {
	t.Helper()
	program := build(t)
	defer func() { _ = program.Close() }()
	driver := open(t, program, "s1")
	defer func() { _ = driver.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, err := range driver.Run(ctx, Request()) {
			_ = err
			break
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled context did not stop the driver")
	}
}

func driverCloseIdempotent(t *testing.T, build ProgramFactory) {
	t.Helper()
	program := build(t)
	driver := open(t, program, "s1")
	if err := driver.Close(); err != nil {
		t.Fatalf("driver Close: %v", err)
	}
	if err := driver.Close(); err != nil {
		t.Fatalf("second driver Close: %v", err)
	}
	if err := program.Close(); err != nil {
		t.Fatalf("program Close: %v", err)
	}
	if err := program.Close(); err != nil {
		t.Fatalf("second program Close: %v", err)
	}
}

// programOpenRepeatable is the two-level shape: opening a second session from the same
// program must work, because a deployment serves many conversations with one program.
func programOpenRepeatable(t *testing.T, build ProgramFactory) {
	t.Helper()
	program := build(t)
	defer func() { _ = program.Close() }()

	first := open(t, program, "s1")
	defer func() { _ = first.Close() }()
	second := open(t, program, "s2")
	defer func() { _ = second.Close() }()
	if first == second {
		t.Fatal("two sessions share one driver; each conversation needs its own")
	}
}

// programConcurrentOpen is the requirement the split creates: one program, many sessions. A
// program that keeps a mutable field without a lock passes every other test and fails here.
func programConcurrentOpen(t *testing.T, build ProgramFactory) {
	t.Helper()
	program := build(t)
	defer func() { _ = program.Close() }()

	const sessions = 8
	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i := range sessions {
		wg.Add(1)
		go func(ordinal int) {
			defer wg.Done()
			driver, err := program.Open(agent.SessionInfo{SessionID: "s" + string(rune('a'+ordinal))})
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = driver.Close() }()
			for _, runErr := range driver.Run(context.Background(), Request()) {
				if runErr != nil {
					errs <- runErr
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Open/Run: %v", err)
	}
}

// drainTurn runs one turn and returns the last terminal state it saw.
func drainTurn(t *testing.T, driver agent.Driver) (agent.TaskState, bool) {
	t.Helper()
	var state agent.TaskState
	for event, err := range driver.Run(context.Background(), Request()) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if status, ok := event.(agent.Status); ok && status.State.Terminal() {
			state = status.State
		}
	}
	return state, state != ""
}

// Request is the smallest request a driver can run: a session binding that allows
// everything, a message, and no store.
func Request() agent.Request {
	return agent.Request{
		Input:   UserMessage("hello"),
		Binding: tooltest.Binding(),
	}
}

// BindingFor is a binding over a named session, for a test that asserts two sessions do not
// share state.
func BindingFor(sessionID string) tool.Binding { return tooltest.BindingFor(sessionID) }

// Completed builds the terminal event every program must end a turn with.
func Completed() agent.Event { return Terminal(agent.StateCompleted) }
