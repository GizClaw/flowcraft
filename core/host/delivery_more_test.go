package host

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// TestADeliveryQueueIsBounded keeps a parent that never runs from turning a delegation into
// unbounded memory: past the cap the oldest message goes to the application.
func TestADeliveryQueueIsBounded(t *testing.T) {
	driver := &deliveryDriver{accepts: true, hold: make(chan struct{}), started: make(chan struct{})}
	var overflow atomic.Int64
	agents := deliveryHost(t, plainDriver{core: driver}, WithDelegationDelivery(
		func(context.Context, string, message.Message) bool {
			overflow.Add(1)
			return true
		}))

	// The first delivery starts a turn that stays in flight, so everything after it queues.
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "first"))
	<-driver.started
	for i := 0; i < maxDeliveryQueue+8; i++ {
		agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "more"))
	}
	if got := overflow.Load(); got == 0 {
		t.Fatalf("no delivery overflowed a queue of %d", maxDeliveryQueue)
	}
	close(driver.hold)
}

// TestASteerRefusalFallsBackToATurn is the seam between the first two channels: a driver that will
// not take a writer at a boundary can still carry the message as a turn's input.
func TestASteerRefusalFallsBackToATurn(t *testing.T) {
	driver := &deliveryDriver{
		accepts:  true,
		steerErr: errdefs.NotAvailablef("graph: steer: state merges by single"),
		hold:     make(chan struct{}),
		started:  make(chan struct{}),
	}
	agents := deliveryHost(t, driver)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-driver.started
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"))
	close(driver.hold)
	<-done
	waitFor(t, "the fallback turn", func() bool { return driver.turns() == 2 })
}

// TestADeliveryMessageCarriesAQuestion covers the paused case: a child that stopped to ask is
// delivered with the question the parent has to answer.
func TestADeliveryMessageCarriesAQuestion(t *testing.T) {
	outcome := platformOutcome()
	question := message.NewTextContent("may I write outside the workspace?")
	outcome.Question = &question
	outcome.Status = string(agent.StateInputRequired)

	msg := deliveryMessage(outcome)
	if got := msg.Content.Text(); got == "" || !strings.Contains(got, "question:") || !strings.Contains(got, "may I write") {
		t.Fatalf("content = %q, want the question", got)
	}
	if got := string(msg.Metadata.Attributes[DeliveryStatusKey]); got != `"input-required"` {
		t.Fatalf("status = %s, want input-required", got)
	}
}

// TestFoldDelegatedKeepsWhatTheParentNeeds pins the fold: text, the paused question and the terminal
// state, and nothing else.
func TestFoldDelegatedKeepsWhatTheParentNeeds(t *testing.T) {
	outcome := platform.DelegationOutcome{}
	folded := &foldedStream{}
	foldDelegated(&outcome, folded, agent.Text{Text: "half"})
	foldDelegated(&outcome, folded, agent.Text{Text: " an answer"})
	foldDelegated(&outcome, folded, agent.Status{State: agent.StateWorking})
	foldDelegated(&outcome, folded, agent.Status{State: agent.StateInputRequired, Reason: "which seat?"})
	foldDelegated(&outcome, folded, agent.Status{State: agent.StateCompleted})
	outcome.Content = folded.content()

	if got := outcome.Content.Text(); got != "half an answer" {
		t.Fatalf("content = %q", got)
	}
	if outcome.Status != string(agent.StateCompleted) {
		t.Fatalf("status = %q, want completed", outcome.Status)
	}
	if outcome.Question == nil || outcome.Question.Text() != "which seat?" {
		t.Fatalf("question = %v", outcome.Question)
	}
}

// TestAHostTurnWaitsForTheApplicationsTurn is the atomic claim: a delivery does not check the slot
// and take it in two steps, so it cannot lose a race it was told to wait out.
func TestAHostTurnWaitsForTheApplicationsTurn(t *testing.T) {
	driver := &deliveryDriver{accepts: true, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, driver)

	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-driver.started

	type claimed struct {
		run func()
		err error
	}
	got := make(chan claimed, 1)
	go func() {
		run, err := agents.turn(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"), turnDelivery, nil)
		var execute func()
		if run != nil {
			execute = run
		}
		got <- claimed{run: execute, err: err}
	}()

	select {
	case <-got:
		t.Fatal("the host turn took the slot while the application's turn was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(driver.hold)
	<-appDone

	var claim claimed
	select {
	case claim = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the host turn never took the slot")
	}
	if claim.err != nil {
		t.Fatalf("turn: %v", claim.err)
	}
	claim.run()
	waitFor(t, "the delivery turn", func() bool { return driver.turns() == 2 })
}

// TestAHostTurnStopsWaitingWhenTheContextEnds keeps a waiter from holding a queue forever when its
// caller gives up.
func TestAHostTurnStopsWaitingWhenTheContextEnds(t *testing.T) {
	driver := &deliveryDriver{accepts: true, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, driver)

	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-driver.started

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := agents.turn(ctx, "s1", message.NewTextMessage(message.RoleUser, "from a child"), turnDelivery, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("turn = %v, want the context's deadline", err)
	}
	close(driver.hold)
	<-appDone
}

// TestAHostTurnStopsWaitingWhenTheSessionCloses is the other way out of the wait: the session goes
// away instead of the turn ending normally.
func TestAHostTurnStopsWaitingWhenTheSessionCloses(t *testing.T) {
	driver := &deliveryDriver{accepts: true, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, driver)

	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-driver.started
	session, _ := agents.Session("s1")

	// A close after the app's turn began: the waiter either has not started yet (and sees closing
	// on its first pass) or is waiting (and wakes on the turn's end) — the answer is the same, so
	// the test does not depend on which one happened.
	if done := session.beginClose(); done != nil {
		<-done
	}
	if err := session.claim(t.Context(), turnDelivery, true); !errdefs.IsNotAvailable(err) {
		t.Fatalf("claim = %v, want NotAvailable once the session is closing", err)
	}
	close(driver.hold)
	<-appDone
}

// TestACallersTurnIsRefusedWhileAnotherRuns pins the conflict's plain wording, which is what a
// caller that competes with its own turn sees.
func TestACallersTurnIsRefusedWhileAnotherRuns(t *testing.T) {
	driver := &deliveryDriver{accepts: true, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, driver)
	go func() {
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "first")) {
		}
	}()
	<-driver.started
	var conflict error
	for _, err := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "second")) {
		conflict = err
		break
	}
	close(driver.hold)
	if conflict == nil || !errdefs.IsConflict(conflict) {
		t.Fatalf("err = %v, want a conflict", conflict)
	}
	if strings.Contains(conflict.Error(), "delegated") {
		t.Fatalf("err = %v, want the plain wording for an application turn", conflict)
	}
}

// TestACallersTurnNamesADelegatedRun is the third kind in the conflict's vocabulary: a caller that
// collides with a delegated child's turn is told which one it is.
func TestACallersTurnNamesADelegatedRun(t *testing.T) {
	driver := &deliveryDriver{accepts: true, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, driver)

	run, err := agents.turn(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "child work"), turnDelegation, nil)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	go run()
	<-driver.started

	var conflict error
	for _, runErr := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		conflict = runErr
		break
	}
	close(driver.hold)
	if conflict == nil || !strings.Contains(conflict.Error(), "delegated run") {
		t.Fatalf("err = %v, want a conflict naming the delegated run", conflict)
	}
}

// perSessionProgram gives every session its own driver, which is what a real program does. The
// shared-driver fixture hides a steer id collision: two graph-backed sessions both mint "graph-1".
type perSessionProgram struct {
	hold    chan struct{}
	started chan *deliveryDriver
}

func (p *perSessionProgram) Open(agent.SessionInfo) (agent.Driver, error) {
	driver := &deliveryDriver{hold: p.hold, started: make(chan struct{})}
	go func() {
		<-driver.started
		p.started <- driver
	}()
	return driver, nil
}

func (p *perSessionProgram) Close() error { return nil }

// TestTwoSessionsKeepTheirOwnSteers is the collision the table-wide map hid: a steer id is minted by
// a driver, and a driver is per session, so the first steer of every graph session is "graph-1".
// Keyed by id alone, one session's bookkeeping overwrote the other's and the message it lost was
// never re-offered — it had already been counted as steered.
func TestTwoSessionsKeepTheirOwnSteers(t *testing.T) {
	hold := make(chan struct{})
	program := &perSessionProgram{hold: hold, started: make(chan *deliveryDriver, 4)}
	var delivered sync.Map // session → message text
	agents := hostFor(t, func() (agent.Program, error) { return program, nil },
		WithDelegationDelivery(func(_ context.Context, sessionID string, msg message.Message) bool {
			delivered.Store(sessionID, msg.Content.Text())
			return true
		}))

	for _, id := range []string{"a", "b"} {
		if _, err := agents.Open(t.Context(), id, "referee", permissiveGrant("local")); err != nil {
			t.Fatalf("Open %s: %v", id, err)
		}
	}
	for _, id := range []string{"a", "b"} {
		go func() {
			for range agents.Run(t.Context(), id, message.NewTextMessage(message.RoleUser, "start")) {
			}
		}()
	}
	for range 2 {
		select {
		case <-program.started:
		case <-time.After(2 * time.Second):
			t.Fatal("a session never started its turn")
		}
	}
	agents.deliver(t.Context(), "a", message.NewTextMessage(message.RoleUser, "for a"))
	agents.deliver(t.Context(), "b", message.NewTextMessage(message.RoleUser, "for b"))

	agents.deliveryMu.Lock()
	tracked := 0
	for _, pending := range agents.steers {
		tracked += len(pending)
	}
	agents.deliveryMu.Unlock()
	if tracked != 2 {
		t.Fatalf("%d steered messages are tracked, want 2 (a driver-local id collided)", tracked)
	}

	// Both turns end without reaching a boundary, so both messages are re-offered. The program cannot
	// take a message between turns, so the application is the one told — which is where the payload
	// says whether the right session kept its own message.
	close(hold)
	waitFor(t, "both re-offers", func() bool {
		_, a := delivered.Load("a")
		_, b := delivered.Load("b")
		return a && b
	})
	for _, id := range []string{"a", "b"} {
		text, _ := delivered.Load(id)
		if want := "for " + id; text != want {
			t.Fatalf("session %s was told %q, want %q", id, text, want)
		}
	}
}

// TestAClosedSessionReportsItsPendingWork keeps a close from swallowing a delivery: the queue and the
// tracked steers of a closed session are handed to the application instead of outliving it.
func TestAClosedSessionReportsItsPendingWork(t *testing.T) {
	driver := &deliveryDriver{accepts: false, hold: make(chan struct{}), started: make(chan struct{})}
	var (
		deliveryMu sync.Mutex
		delivered  []string
	)
	agents := hostFor(t, func() (agent.Program, error) { return &deliveryProgram{driver: driver}, nil },
		WithDelegationDelivery(func(_ context.Context, _ string, msg message.Message) bool {
			deliveryMu.Lock()
			defer deliveryMu.Unlock()
			delivered = append(delivered, msg.Content.Text())
			return true
		}))
	close(driver.hold)

	agents.enqueueDelivery(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "queued"))
	agents.trackSteer("s1", "graph-1", message.NewTextMessage(message.RoleUser, "steered"))

	if err := agents.CloseSession(t.Context(), "s1"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	waitFor(t, "the pending work", func() bool {
		deliveryMu.Lock()
		defer deliveryMu.Unlock()
		return len(delivered) == 2
	})
	agents.deliveryMu.Lock()
	queues, steers := len(agents.delivery), len(agents.steers)
	agents.deliveryMu.Unlock()
	if queues != 0 || steers != 0 {
		t.Fatalf("the closed session left %d queues and %d steer maps behind", queues, steers)
	}
}
