package host

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Delivery is how work that arrived somewhere else reaches a conversation.
//
// A delegated run finishes, or a subagent asks the agent that delegated to it, while the parent
// may be busy, idle, or unable to accept a message at all. Three channels answer that, in this
// order, and each has exactly one consumer of the resulting events:
//
//   - Steering: the parent has a turn in flight and its driver can take input. The message joins
//     that turn, and the caller that already consumes the turn's iterator sees it. No new turn,
//     no second source of events.
//   - A turn the host starts: the parent is idle (or its driver cannot be steered, but can still
//     take a message between turns). The host runs the turn and forwards its events to
//     [WithHostTurnEvents]; a process that installed no sink gets the record only, and the
//     transcript in the checkpoint.
//   - The application's own channel: the parent is gone, or its program cannot accept a message
//     between turns. [WithDelegationDelivery] is told, and the application decides what to do.
//
// Nothing here can start a second concurrent turn: the session's own slot decides that, and a
// delivery waits for it.

// HostTurnKind names why the host started a turn itself.
type HostTurnKind string

const (
	// HostTurnDelivery is a turn started to hand a session work that arrived while it was idle.
	HostTurnDelivery HostTurnKind = "delivery"
	// HostTurnDelegation is the host running a delegated child on the caller's behalf.
	HostTurnDelegation HostTurnKind = "delegation"
)

// HostTurnEvents receives the events of a turn the host started.
//
// It is called only for those turns: a turn the application began through [Agents.Run] is read
// from the iterator it was handed, and this sink sees nothing of it. That is the rule that keeps
// one event from having two consumers.
//
// The callback runs on the goroutine driving the turn, and returns whether to keep reading; false
// cancels the rest of the turn. It must not block and must not call [Agents.Run] for the same
// session. Different sessions may deliver concurrently, so an implementation shared by several
// sessions has to be safe for concurrent use.
type HostTurnEvents func(ctx context.Context, sessionID string, kind HostTurnKind, event agent.Event, err error) bool

// DelegationDelivery receives a message the host could not put into a session's conversation.
//
// It is the last channel, and the only one that exists when the program declares no way to take a
// message between turns — a graph with no input slot, for instance. Returning false reports that
// the application did not take it either; the host only records that.
type DelegationDelivery func(ctx context.Context, sessionID string, msg message.Message) bool

// WithHostTurnEvents installs the sink for turns the host starts itself.
//
// Without it those turns still run — the work they carry is the reason they exist — but their
// events are discarded and only the run record and the checkpoint remain.
func WithHostTurnEvents(sink HostTurnEvents) AgentsOption {
	return func(a *Agents) { a.hostTurnEvents = sink }
}

// WithDelegationDelivery installs the channel for messages the host cannot deliver.
func WithDelegationDelivery(sink DelegationDelivery) AgentsOption {
	return func(a *Agents) { a.delegationDelivery = sink }
}

// deliveryQueue is one session's pending messages, oldest first.
//
// There is one per session and one worker draining it, so two children finishing at once cannot
// start two turns: the second waits in the queue. The queue is bounded — a parent that never
// runs must not turn a delegation into unbounded memory — and the oldest is dropped with a
// warning when it overflows.
type deliveryQueue struct {
	mu       sync.Mutex
	items    []deliveryItem
	draining bool
}

// deliveryItem is one message waiting for a turn.
type deliveryItem struct {
	msg message.Message
}

// maxDeliveryQueue bounds one session's pending deliveries.
const maxDeliveryQueue = 32

// deliver hands msg to the session, by the first channel that can take it.
//
// It is called from a delegated child's goroutine, so it must not assume the parent is still open
// or idle. The context should be detached from the child's: the parent's delivery is not part of
// the child's turn.
func (a *Agents) deliver(ctx context.Context, sessionID string, msg message.Message) {
	session, ok := a.Session(sessionID)
	if !ok {
		a.undelivered(ctx, sessionID, msg, "the session is not open")
		return
	}
	if err := a.steerDelivery(ctx, session, msg); err == nil {
		recordDelivery(ctx, session.agent, outcomeSteered)
		return
	} else if !errors.Is(err, errNotSteerable) && !errors.Is(err, agent.ErrSessionIdle) {
		// A driver may refuse the message for a reason a turn does not share: a graph whose input
		// slot merges by "single" will not take a writer at a boundary, and still seeds a turn's
		// input. Only a program that can take nothing between turns goes to the application.
		driver, _ := session.driverSnapshot()
		if !acceptsInput(driver) {
			a.undelivered(ctx, sessionID, msg, err.Error())
			return
		}
	}
	a.enqueueDelivery(ctx, sessionID, msg)
}

// errNotSteerable reports a parent that cannot take input mid-turn; it is not a failure of the
// delivery, only a reason to use the next channel.
var errNotSteerable = errors.New("host: the session cannot be steered")

// steerDelivery offers msg to a running turn, and reports why it could not.
func (a *Agents) steerDelivery(ctx context.Context, session *Session, msg message.Message) error {
	if !session.isRunning() {
		return agent.ErrSessionIdle
	}
	driver, _ := session.driverSnapshot()
	steerer, ok := driver.(agent.Steerer)
	if !ok {
		return errNotSteerable
	}
	input := agent.SteerInput{Message: msg, Intent: agent.SteerDeliver}
	id, err := steerer.Steer(input)
	if err != nil {
		if errors.Is(err, agent.ErrSessionIdle) {
			return agent.ErrSessionIdle
		}
		return err
	}
	// The turn may end before a boundary: the driver reports that as a dropped input, and the
	// host re-offers it once the turn is over. Keeping the message here is what makes the retry
	// possible, because the drop report carries only the handle.
	a.trackSteer(session.id, id, msg)
	recordSteer(ctx, input, steerAccepted)
	return nil
}

// enqueueDelivery queues msg for a turn the host will start, and makes sure a worker is draining.
func (a *Agents) enqueueDelivery(ctx context.Context, sessionID string, msg message.Message) {
	a.deliveryMu.Lock()
	queue, ok := a.delivery[sessionID]
	if !ok {
		queue = &deliveryQueue{}
		a.delivery[sessionID] = queue
	}
	a.deliveryMu.Unlock()

	queue.mu.Lock()
	if len(queue.items) >= maxDeliveryQueue {
		dropped := queue.items[0]
		queue.items = queue.items[1:]
		queue.mu.Unlock()
		a.undelivered(ctx, sessionID, dropped.msg, "the delivery queue is full")
		queue.mu.Lock()
	}
	queue.items = append(queue.items, deliveryItem{msg: msg})
	if queue.draining {
		queue.mu.Unlock()
		return
	}
	queue.draining = true
	queue.mu.Unlock()

	a.workers.Add(1)
	go a.drainDelivery(sessionID, queue)
}

// drainDelivery runs the queued messages of one session in order.
func (a *Agents) drainDelivery(sessionID string, queue *deliveryQueue) {
	defer a.workers.Done()
	for {
		queue.mu.Lock()
		if len(queue.items) == 0 {
			queue.draining = false
			queue.mu.Unlock()
			return
		}
		item := queue.items[0]
		queue.items = queue.items[1:]
		queue.mu.Unlock()

		a.runDelivery(sessionID, item)
	}
}

// runDelivery runs one queued message as a turn, or tells the application it could not.
func (a *Agents) runDelivery(sessionID string, item deliveryItem) {
	// Detached: the child that produced this may be gone, and the delivery is the parent's work.
	ctx := context.WithoutCancel(context.Background())
	session, ok := a.Session(sessionID)
	if !ok {
		a.undelivered(ctx, sessionID, item.msg, "the session is not open")
		return
	}
	driver, _ := session.driverSnapshot()
	if !acceptsInput(driver) {
		a.undelivered(ctx, sessionID, item.msg, "the program cannot take a message between turns")
		return
	}
	// The turn waits for the slot instead of checking it first: taking the slot is what the wait
	// was for, so it is one decision, and a caller's turn cannot slip in between them.
	started, err := a.turn(ctx, sessionID, item.msg, turnDelivery, a.hostTurnEvents)
	if err != nil {
		a.undelivered(ctx, sessionID, item.msg, err.Error())
		return
	}
	recordDelivery(ctx, session.agent, outcomeTurn)
	started()
}

// acceptsInput reports whether a driver can take a message outside a turn.
//
// A driver that does not say is treated as unable: silently dropping a delivery is worse than
// telling the application about it.
func acceptsInput(driver agent.Driver) bool {
	sink, ok := driver.(agent.InputSink)
	return ok && sink.AcceptsInput()
}

// undelivered hands a message to the application, or records that nobody took it.
func (a *Agents) undelivered(ctx context.Context, sessionID string, msg message.Message, reason string) {
	if a.delegationDelivery != nil && a.delegationDelivery(ctx, sessionID, msg) {
		recordDelivery(ctx, "", outcomeCallback)
		return
	}
	recordDelivery(ctx, "", outcomeUndelivered)
	telemetry.Warn(ctx, "host: a delivery had nowhere to go",
		telemetry.String("session.id", sessionID),
		telemetry.String("reason", reason))
}

// The outcomes host.deliveries counts.
const (
	outcomeSteered     = "steered"
	outcomeTurn        = "turn"
	outcomeCallback    = "callback"
	outcomeUndelivered = "undelivered"
)

// The steer bookkeeping: a message offered to a running turn is only delivered once the turn
// reaches a boundary and the driver reports the input as applied. Until then it is pending, and a
// turn that ends without it puts it back in the queue.

// trackedSteer is one message waiting to be applied by a running turn.
type trackedSteer struct {
	msg message.Message
}

// trackSteer records a steered message so a turn that ends before a boundary can re-offer it.
//
// The map is keyed by session *and* id because a steer id is minted by a driver, and a driver is per
// session: two graph-backed sessions both name their first steer "graph-1". A table-wide id map would
// have one conversation's bookkeeping overwrite another's, and the message it lost would never be
// re-offered — it was already counted as steered.
func (a *Agents) trackSteer(sessionID string, id agent.SteerID, msg message.Message) {
	a.deliveryMu.Lock()
	defer a.deliveryMu.Unlock()
	pending, ok := a.steers[sessionID]
	if !ok {
		pending = map[agent.SteerID]trackedSteer{}
		a.steers[sessionID] = pending
	}
	pending[id] = trackedSteer{msg: msg}
}

// steerApplied drops a steered message the driver applied: it is in the transcript now.
func (a *Agents) steerApplied(sessionID string, id agent.SteerID) {
	a.deliveryMu.Lock()
	defer a.deliveryMu.Unlock()
	pending, ok := a.steers[sessionID]
	if !ok {
		return
	}
	delete(pending, id)
	if len(pending) == 0 {
		delete(a.steers, sessionID)
	}
}

// requeueDropped puts every unapplied steered message of one session back in its queue.
//
// It runs when a turn ends, which is the only moment the answer is known: a message that reached
// a boundary was reported as applied, and one that did not is still here.
func (a *Agents) requeueDropped(ctx context.Context, sessionID string) {
	a.deliveryMu.Lock()
	pending := a.steers[sessionID]
	dropped := make([]trackedSteer, 0, len(pending))
	for _, steer := range pending {
		dropped = append(dropped, steer)
	}
	delete(a.steers, sessionID)
	a.deliveryMu.Unlock()
	for _, steer := range dropped {
		telemetry.Debug(ctx, "host: a steered delivery did not reach a boundary",
			telemetry.String("session.id", sessionID))
		a.enqueueDelivery(ctx, sessionID, steer.msg)
	}
}

// forgetDelivery releases a closed session's pending work and tells the application about it.
//
// Without it the queue and the tracked steers of a session id outlive the session: the table grows
// one entry per conversation a process ever served, and a message steered just before the close is
// neither re-offered by a later turn (there is none) nor handed to the application.
func (a *Agents) forgetDelivery(ctx context.Context, sessionID string) {
	a.deliveryMu.Lock()
	queue := a.delivery[sessionID]
	delete(a.delivery, sessionID)
	pending := a.steers[sessionID]
	delete(a.steers, sessionID)
	a.deliveryMu.Unlock()

	dropped := make([]message.Message, 0, len(pending))
	for _, steer := range pending {
		dropped = append(dropped, steer.msg)
	}
	if queue != nil {
		queue.mu.Lock()
		for _, item := range queue.items {
			dropped = append(dropped, item.msg)
		}
		queue.items = nil
		queue.mu.Unlock()
	}
	for _, msg := range dropped {
		a.undelivered(ctx, sessionID, msg, "the session closed")
	}
}

// recordDelivery counts one delivery by what became of it.
func recordDelivery(ctx context.Context, agentName, outcome string) {
	meter := telemetry.MeterWithSuffix("host")
	counter, err := meter.Int64Counter("host.deliveries",
		metric.WithDescription("Messages handed to a session by how they arrived"))
	if err != nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("outcome", outcome)}
	if agentName != "" {
		attrs = append(attrs, attribute.String(identity.AttrAgentName, agentName))
	}
	counter.Add(ctx, 1, metric.WithAttributes(attrs...))
}
