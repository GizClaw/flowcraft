package host

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// The delivery path has three costs worth watching: building the message that carries a child's
// result, queueing it when no turn can take it, and handing it to a running turn. A regression in
// any of them is paid once per delegated run, which is cheap — but the steer path is on the
// parent's turn goroutine, so a lock or an allocation there delays a conversation.

// benchDeliveryOutcome is what a finished child hands the parent.
func benchDeliveryOutcome() platform.DelegationOutcome {
	question := message.NewTextContent("may I write outside the workspace?")
	return platform.DelegationOutcome{
		Session:  "referee/seat_1-7",
		Agent:    "seat_1",
		Chain:    "chain-0123456789abcdef",
		Depth:    2,
		Status:   string(agent.StateInputRequired),
		Content:  message.NewTextContent("the vote is recorded, and one seat asked to write a note"),
		Question: &question,
	}
}

// BenchmarkDeliveryMessage is what one delivery costs before it is handed over: the summary a
// reader sees and the metadata an application groups by.
func BenchmarkDeliveryMessage(b *testing.B) {
	outcome := benchDeliveryOutcome()
	b.ReportAllocs()
	for b.Loop() {
		msg := deliveryMessage(outcome)
		if msg.Metadata == nil {
			b.Fatal("no metadata")
		}
	}
}

// BenchmarkEnqueueDelivery is the queue push alone: a worker is already draining, so nothing starts
// a turn and the measurement is the lock and the append.
func BenchmarkEnqueueDelivery(b *testing.B) {
	agents := deliveryHost(b, &deliveryDriver{accepts: true})
	queue := &deliveryQueue{draining: true}
	agents.deliveryMu.Lock()
	agents.delivery["s1"] = queue
	agents.deliveryMu.Unlock()
	msg := message.NewTextMessage(message.RoleUser, "from a child")

	b.ReportAllocs()
	for b.Loop() {
		agents.enqueueDelivery(context.Background(), "s1", msg)
		// Empty the queue without letting the "draining" flag change, so each iteration measures
		// the same push: the worker this flag claims to be running is the measurement's fiction.
		queue.mu.Lock()
		queue.items = queue.items[:0]
		queue.mu.Unlock()
	}
}

// BenchmarkDeliverSteersARunningTurn is the first channel's cost: a lookup, the driver's Steer and
// the bookkeeping that lets a dropped message be offered again.
//
// The tracked steers are cleared every iteration because the measurement is one delivery, and a
// map that grew with the loop would price the harness instead.
func BenchmarkDeliverSteersARunningTurn(b *testing.B) {
	driver := &deliveryDriver{accepts: true, applySteer: false, hold: make(chan struct{}), started: make(chan struct{})}
	agents := deliveryHost(b, driver)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(context.Background(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-driver.started
	msg := message.NewTextMessage(message.RoleUser, "from a child")
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		agents.deliver(ctx, "s1", msg)
		agents.deliveryMu.Lock()
		clear(agents.steers)
		agents.deliveryMu.Unlock()
		driver.dropSteers()
	}
	close(driver.hold)
	<-done
}

// BenchmarkDeliverRunsATurn is the idle channel end to end: the message is queued, the worker waits
// for the slot, and a turn runs the conversation's reaction. The driver is a stub, so the number is
// this layer's overhead rather than a model's.
func BenchmarkDeliverRunsATurn(b *testing.B) {
	driver := &deliveryDriver{accepts: true}
	agents := deliveryHost(b, driver)
	session, _ := agents.Session("s1")
	msg := message.NewTextMessage(message.RoleUser, "from a child")
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		before := driver.turns()
		agents.deliver(ctx, "s1", msg)
		// The turn has to be over, not only started: the next iteration's delivery would
		// otherwise steer it instead of starting one, and the loop would wait for a turn that
		// joined another.
		waitTurn(b, session, driver, before+1)
	}
}

// dropSteers forgets what the driver accepted, so a benchmark loop does not accumulate.
func (d *deliveryDriver) dropSteers() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.steers = nil
}

// waitTurn waits for the delivery worker to finish one turn, so a benchmark iteration measures the
// whole hand-over rather than the queue push before it.
func waitTurn(b *testing.B, session *Session, driver *deliveryDriver, want int) {
	b.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for driver.turns() < want || session.isRunning() {
		if time.Now().After(deadline) {
			b.Fatalf("the delivery turn never finished: turns = %d", driver.turns())
		}
		// The worker is another goroutine; a tight loop here would take the core it needs.
		runtime.Gosched()
	}
}
