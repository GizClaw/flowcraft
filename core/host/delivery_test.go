package host

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
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

// queuedSteer is one input the test driver accepted, with the handle it returned.
type queuedSteer struct {
	id    agent.SteerID
	input agent.SteerInput
}

// deliveryDriver is the conversation a delivery test talks to.
//
// It records the turns it ran and the inputs it was handed, and it can hold a turn open so a
// delivery arrives while that turn is in flight — the state the steer channel exists for.
type deliveryDriver struct {
	mu       sync.Mutex
	requests []agent.Request
	steers   []queuedSteer
	nextID   atomic.Int64
	// applySteer makes the boundary report queued inputs as applied; false is a driver whose
	// turn ends before it reaches one, which is what drops a delivery.
	applySteer bool
	// accepts is what the driver says about a message offered between turns.
	accepts bool
	// steerErr makes Steer refuse, which is what a driver whose input slot cannot take a writer
	// does at a boundary while a turn can still carry the message.
	steerErr error
	// hold keeps a turn open until the test releases it, and started is closed when one begins.
	hold    chan struct{}
	started chan struct{}
}

func (d *deliveryDriver) Run(ctx context.Context, req agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		d.mu.Lock()
		d.requests = append(d.requests, req)
		hold, started := d.hold, d.started
		d.mu.Unlock()
		if started != nil {
			close(started)
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
			}
		}
		// A boundary: what was steered while the turn ran is applied here.
		d.mu.Lock()
		queued := d.steers
		d.steers = nil
		d.mu.Unlock()
		for _, steer := range queued {
			if !d.applySteer {
				continue
			}
			yield(agent.Interrupted{Interruption: agent.Interruption{
				ID:     steer.id,
				Intent: steer.input.Intent,
			}}, nil)
		}
		yield(agent.Text{Text: "ok"}, nil)
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (d *deliveryDriver) Close() error { return nil }

func (d *deliveryDriver) AcceptsInput() bool { return d.accepts }

func (d *deliveryDriver) Steer(input agent.SteerInput) (agent.SteerID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.steerErr != nil {
		return "", d.steerErr
	}
	id := agent.SteerID(fmt.Sprintf("steer-%d", d.nextID.Add(1)))
	d.steers = append(d.steers, queuedSteer{id: id, input: input})
	return id, nil
}

func (d *deliveryDriver) Unsteer(agent.SteerID) error { return nil }

// queued returns the inputs the driver has not consumed.
func (d *deliveryDriver) queued() []queuedSteer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]queuedSteer(nil), d.steers...)
}

// turns returns how many turns the driver ran.
func (d *deliveryDriver) turns() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

// deliveryProgram hands the test's driver to every session it opens.
type deliveryProgram struct{ driver agent.Driver }

func (p *deliveryProgram) Open(agent.SessionInfo) (agent.Driver, error) { return p.driver, nil }
func (p *deliveryProgram) Close() error                                 { return nil }

// plainDriver is the same conversation without the steering interface: a driver that only hears
// from the application.
type plainDriver struct{ core *deliveryDriver }

func (d plainDriver) Run(ctx context.Context, req agent.Request) iter.Seq2[agent.Event, error] {
	return d.core.Run(ctx, req)
}
func (plainDriver) Close() error         { return nil }
func (d plainDriver) AcceptsInput() bool { return true }

// deliveryHost installs one agent over the given driver and opens its session.
//
// It takes testing.TB so a benchmark can use the same wiring: the setup is the same one, and a
// second copy of it would be the one that drifts.
func deliveryHost(t testing.TB, driver agent.Driver, opts ...AgentsOption) *Agents {
	t.Helper()
	return hostFor(t, func() (agent.Program, error) { return &deliveryProgram{driver: driver}, nil }, opts...)
}

// waitFor polls until cond holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestADeliveryRunsATurnWhenTheSessionIsIdle is the idle half of the delivery channel: the parent
// has no turn to join, so the host starts one and hands its events to the sink.
func TestADeliveryRunsATurnWhenTheSessionIsIdle(t *testing.T) {
	driver := &deliveryDriver{accepts: true}
	var events atomic.Int64
	var kind HostTurnKind
	agents := deliveryHost(t, driver, WithHostTurnEvents(func(_ context.Context, _ string, k HostTurnKind, _ agent.Event, _ error) bool {
		kind = k
		events.Add(1)
		return true
	}))
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"))

	waitFor(t, "the delivery turn", func() bool { return driver.turns() == 1 })
	waitFor(t, "its events", func() bool { return events.Load() > 0 })
	if kind != HostTurnDelivery {
		t.Fatalf("kind = %q, want %q", kind, HostTurnDelivery)
	}
}

// TestTheHostSinkSeesNothingOfAnApplicationTurn is the rule that keeps one event from having two
// consumers: a turn the caller started is read from its own iterator, and the sink stays silent.
func TestTheHostSinkSeesNothingOfAnApplicationTurn(t *testing.T) {
	driver := &deliveryDriver{accepts: true}
	forwarded := 0
	agents := deliveryHost(t, driver, WithHostTurnEvents(func(context.Context, string, HostTurnKind, agent.Event, error) bool {
		forwarded++
		return true
	}))
	consumed := 0
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		consumed++
	}
	if consumed == 0 {
		t.Fatal("the application's iterator saw no events")
	}
	if forwarded != 0 {
		t.Fatalf("the host sink forwarded %d events of an application turn", forwarded)
	}
}

// TestADeliverySteersARunningTurn is the first channel: a parent that is running gets the message
// in its own turn, and no second turn is started.
func TestADeliverySteersARunningTurn(t *testing.T) {
	driver := &deliveryDriver{accepts: true, applySteer: true, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, driver)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-driver.started
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"))
	waitFor(t, "the steered input", func() bool { return len(driver.queued()) == 1 })
	if got := driver.queued()[0].input.Intent; got != agent.SteerDeliver {
		t.Fatalf("intent = %q, want %q", got, agent.SteerDeliver)
	}
	close(driver.hold)
	<-done

	// The steer reached a boundary, so it is in the transcript: no second turn is needed.
	time.Sleep(20 * time.Millisecond)
	if got := driver.turns(); got != 1 {
		t.Fatalf("turns = %d, want the delivery to have joined the first", got)
	}
}

// TestADroppedSteerIsOfferedAgainAsATurn is the reliability half: a turn that ends before applying
// the input re-offers it, and the host starts a turn for it.
func TestADroppedSteerIsOfferedAgainAsATurn(t *testing.T) {
	driver := &deliveryDriver{accepts: true, applySteer: false, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, driver)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-driver.started
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"))
	waitFor(t, "the steered input", func() bool { return len(driver.queued()) == 1 })
	close(driver.hold)
	<-done

	waitFor(t, "the re-offered delivery turn", func() bool { return driver.turns() == 2 })
}

// TestADeliveryToAProgramThatCannotTakeInputGoesToTheApplication is the last channel: a
// conversation that cannot hold a message between turns must not silently drop one.
func TestADeliveryToAProgramThatCannotTakeInputGoesToTheApplication(t *testing.T) {
	driver := &deliveryDriver{accepts: false}
	called := make(chan string, 1)
	agents := deliveryHost(t, driver, WithDelegationDelivery(func(_ context.Context, sessionID string, msg message.Message) bool {
		called <- sessionID + ": " + msg.Content.Text()
		return true
	}))
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "undeliverable"))

	select {
	case got := <-called:
		if got != "s1: undeliverable" {
			t.Fatalf("callback = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the application was never told")
	}
	if driver.turns() != 0 {
		t.Fatalf("the driver ran %d turns for a message it cannot take", driver.turns())
	}
}

// TestADeliveryToAClosedSessionGoesToTheApplication covers the parent that went away while its child
// was still working.
func TestADeliveryToAClosedSessionGoesToTheApplication(t *testing.T) {
	driver := &deliveryDriver{accepts: true}
	called := make(chan string, 1)
	agents := deliveryHost(t, driver, WithDelegationDelivery(func(_ context.Context, sessionID string, _ message.Message) bool {
		called <- sessionID
		return true
	}))
	agents.deliver(t.Context(), "missing", message.NewTextMessage(message.RoleUser, "late"))
	select {
	case got := <-called:
		if got != "missing" {
			t.Fatalf("callback = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the application was never told")
	}
}

// TestTheConflictNamesTheHostsTurn is the readable half of "one turn at a time": a caller that
// collides with a turn the host started is told which one, because retrying is the answer.
func TestTheConflictNamesTheHostsTurn(t *testing.T) {
	driver := &deliveryDriver{accepts: true, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, driver)
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"))
	<-driver.started

	var conflict error
	for _, err := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			conflict = err
			break
		}
	}
	close(driver.hold)
	if conflict == nil || !strings.Contains(conflict.Error(), "delegated delivery") {
		t.Fatalf("err = %v, want a conflict naming the delivery turn", conflict)
	}
}

// platformOutcome is what a finished child hands the parent.
func platformOutcome() platform.DelegationOutcome {
	return platform.DelegationOutcome{
		Session: "parent/seat-1",
		Agent:   "seat_1",
		Chain:   "chain-abc",
		Depth:   2,
		Status:  "completed",
		Content: message.NewTextContent("done"),
	}
}

// TestADeliveryToANonSteerableTurnIsQueued is the second channel's other door: a parent that is
// running but cannot be steered waits for its turn to end, then gets a turn of its own.
func TestADeliveryToANonSteerableTurnIsQueued(t *testing.T) {
	core := &deliveryDriver{accepts: true, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(t, plainDriver{core: core})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-core.started
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"))
	close(core.hold)
	<-done
	waitFor(t, "the queued delivery turn", func() bool { return core.turns() == 2 })
}

// TestADeliveryThatNoChannelCanTakeGoesToTheApplication is the fail-closed corner: the driver
// refuses the boundary and cannot take a turn's message either, so the application is told.
func TestADeliveryThatNoChannelCanTakeGoesToTheApplication(t *testing.T) {
	driver := &deliveryDriver{
		accepts:  false,
		steerErr: errdefs.NotAvailablef("graph: steer: the document declares no input slot"),
		hold:     make(chan struct{}),
		started:  make(chan struct{}),
	}
	called := make(chan string, 1)
	agents := deliveryHost(t, driver, WithDelegationDelivery(func(_ context.Context, sessionID string, _ message.Message) bool {
		called <- sessionID
		return true
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-driver.started
	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"))
	select {
	case got := <-called:
		if got != "s1" {
			t.Fatalf("callback = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the application was never told")
	}
	close(driver.hold)
	<-done
	if got := driver.turns(); got != 1 {
		t.Fatalf("turns = %d, want only the application's", got)
	}
}

// TestTheWorkerGivesUpOnASessionThatClosed covers the queue that outlives its session: a message
// queued while the parent was open and drained after it closed goes to the application.
func TestTheWorkerGivesUpOnASessionThatClosed(t *testing.T) {
	called := make(chan string, 1)
	agents := deliveryHost(t, &deliveryDriver{accepts: true}, WithDelegationDelivery(
		func(_ context.Context, sessionID string, _ message.Message) bool {
			called <- sessionID
			return true
		}))
	// Drive the worker's first branch directly: the queue exists, the session does not.
	agents.runDelivery("missing", deliveryItem{msg: message.NewTextMessage(message.RoleUser, "late")})
	select {
	case got := <-called:
		if got != "missing" {
			t.Fatalf("callback = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the application was never told")
	}
}

// TestTheWorkerStopsOnAClosingSession is the waitIdle corner: a message queued for a session that
// is already on its way out goes to the application rather than starting a turn on a dead boundary.
func TestTheWorkerStopsOnAClosingSession(t *testing.T) {
	var called atomic.Value
	agents := deliveryHost(t, &deliveryDriver{accepts: true}, WithDelegationDelivery(
		func(_ context.Context, sessionID string, _ message.Message) bool {
			called.Store(sessionID)
			return true
		}))
	session, _ := agents.Session("s1")
	// beginClose marks the session closing while it is still in the table, which is the window a
	// queued delivery can land in.
	if done := session.beginClose(); done != nil {
		<-done
	}
	agents.runDelivery("s1", deliveryItem{msg: message.NewTextMessage(message.RoleUser, "late")})
	if got, _ := called.Load().(string); got != "s1" {
		t.Fatalf("callback = %q, want s1", got)
	}
}

// TestHostTurnKindNamesTheTurn keeps the sink's vocabulary honest for both kinds.
func TestHostTurnKindNamesTheTurn(t *testing.T) {
	for kind, want := range map[turnKind]HostTurnKind{
		turnDelivery:   HostTurnDelivery,
		turnDelegation: HostTurnDelegation,
	} {
		if got := hostTurnKind(kind); got != want {
			t.Fatalf("hostTurnKind(%d) = %q, want %q", kind, got, want)
		}
	}
}

// TestADeliveryMessageWithoutAnAnswer keeps the text honest for a run that produced none.
func TestADeliveryMessageWithoutAnAnswer(t *testing.T) {
	outcome := platformOutcome()
	outcome.Content = message.Content{}
	msg := deliveryMessage(outcome)
	if got := msg.Content.Text(); strings.Contains(got, "answer:") || strings.Contains(got, "question:") {
		t.Fatalf("content = %q, want no answer or question section", got)
	}
	if got := string(msg.Metadata.Attributes[DeliverySessionKey]); got != `"parent/seat-1"` {
		t.Fatalf("session attribute = %s", got)
	}
}

// TestADeliveryCarriesItsFactsInTheMetadata is the machine-readable half of a delivery: the text
// says what happened, and the metadata says which run it was, so an application can route or
// group it without parsing a sentence.
func TestADeliveryCarriesItsFactsInTheMetadata(t *testing.T) {
	msg := deliveryMessage(platformOutcome())
	if msg.Metadata == nil {
		t.Fatal("the delivery message carries no metadata")
	}
	for key, want := range map[string]string{
		DeliverySessionKey: `"parent/seat-1"`,
		DeliveryAgentKey:   `"seat_1"`,
		DeliveryChainKey:   `"chain-abc"`,
		DeliveryDepthKey:   `2`,
		DeliveryStatusKey:  `"completed"`,
	} {
		got, ok := msg.Metadata.Attributes[key]
		if !ok {
			t.Fatalf("metadata carries no %q: %+v", key, msg.Metadata.Attributes)
		}
		if string(got) != want {
			t.Fatalf("%q = %s, want %s", key, got, want)
		}
	}
	if got := msg.Content.Text(); !strings.Contains(got, "parent/seat-1") || !strings.Contains(got, "completed") {
		t.Fatalf("content = %q, want the human-readable summary", got)
	}
	var depth int
	if err := json.Unmarshal(msg.Metadata.Attributes[DeliveryDepthKey], &depth); err != nil || depth != 2 {
		t.Fatalf("depth = %d (%v), want 2", depth, err)
	}
}
