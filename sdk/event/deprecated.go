// Deprecated event API — retained for source compatibility during the
// v0.1.x → v0.2.0 transition. This entire file is scheduled for deletion
// in v0.2.0; per-symbol Migration godoc gives the new-API equivalent.
//
// Why a dedicated file: keeping every Deprecated symbol in one place makes
// the v0.2.0 removal a single `git rm sdk/event/deprecated.go`, with the
// only follow-up being the matching test file. New code MUST NOT add to
// this file; new code belongs in bus.go / memory.go / envelope.go /
// subject.go / observer.go alongside the new Bus surface.

package event

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/xid"
	"go.opentelemetry.io/otel/trace"
)

// EventType identifies the kind of event.
//
// Deprecated: this name is retained for source compatibility during the
// v0.1.x → v0.2.0 transition. The new API uses Subject (a dot-delimited
// routing key) instead.
//
// Migration:
//
//	old: var t event.EventType = event.EventNodeStart
//	new: var s event.Subject   = "graph.run.r1.node.n1.start"
//
// The well-known constants below (EventNodeStart, EventTaskSubmitted, …)
// remain importable but should be replaced with the corresponding subject
// helpers exposed by each producing package.
type EventType string

// Built-in EventType constants used by graph executor and SDK consumers.
//
// Deprecated: see EventType.
const (
	EventGraphStart         EventType = "graph.start"
	EventGraphEnd           EventType = "graph.end"
	EventNodeStart          EventType = "node.start"
	EventNodeComplete       EventType = "node.complete"
	EventNodeError          EventType = "node.error"
	EventNodeSkipped        EventType = "node.skipped"
	EventCheckpoint         EventType = "checkpoint"
	EventStreamDelta        EventType = "stream.delta"
	EventParallelFork       EventType = "parallel.fork"
	EventParallelJoin       EventType = "parallel.join"
	EventKanbanUpdate       EventType = "kanban.update"
	EventApprovalRequired   EventType = "approval.required"
	EventGraphChanged       EventType = "graph.changed"
	EventAgentConfigChanged EventType = "agent_config.changed"
	EventCompileResult      EventType = "compile.result"
)

// Event is a structured event emitted during graph execution.
//
// Deprecated: use Envelope. Removed in v0.2.0.
//
// Migration:
//
//	old: bus.Publish(ctx, event.Event{
//	         Type:   event.EventNodeStart,
//	         RunID:  "r1",
//	         NodeID: "n1",
//	         Payload: map[string]any{"foo": 1},
//	     })
//	new: env, err := event.NewEnvelope(ctx,
//	         event.Subject("graph.run.r1.node.n1.start"),
//	         map[string]any{"foo": 1})
//	     if err != nil { return err }
//	     env.SetRunID("r1")
//	     env.SetNodeID("n1")
//	     bus.Publish(ctx, env)
//
// Field mapping when porting consumers:
//
//	old field          new location
//	-----------------  --------------------------------
//	Event.Type         encoded in Envelope.Subject
//	Event.RunID        Envelope.Header(event.HeaderRunID)   / Envelope.RunID()
//	Event.NodeID       Envelope.Header(event.HeaderNodeID)  / Envelope.NodeID()
//	Event.ActorID      Envelope.Header(event.HeaderActorID) / Envelope.ActorID()
//	Event.GraphID      Envelope.Header(event.HeaderGraphID) / Envelope.GraphID()
//	Event.Payload      Envelope.Payload (json.RawMessage; use Envelope.Decode)
//	Event.TraceID/SpanID/Timestamp/ID  same-named Envelope fields
type Event struct {
	ID        string    `json:"id"`
	Type      EventType `json:"type"`
	RunID     string    `json:"run_id,omitempty"`
	GraphID   string    `json:"graph_id,omitempty"`
	ActorID   string    `json:"actor_id,omitempty"`
	NodeID    string    `json:"node_id,omitempty"`
	TraceID   string    `json:"trace_id,omitempty"`
	SpanID    string    `json:"span_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Payload   any       `json:"payload,omitempty"`
}

// EventFilter specifies criteria for subscribing to events.
//
// Deprecated: use Pattern + WithPredicate. Removed in v0.2.0.
//
// Migration:
//
//	old: sub, _ := bus.Subscribe(ctx, event.EventFilter{
//	         Types:  []event.EventType{event.EventNodeStart, event.EventNodeComplete},
//	         RunID:  "r1",
//	         NodeID: "n1",
//	     })
//	new: sub, _ := bus.Subscribe(ctx,
//	         event.Pattern("graph.run.r1.node.n1.*"),
//	         event.WithPredicate(func(env event.Envelope) bool {
//	             // Pattern already restricts to run/node; further filter by
//	             // suffix when needed.
//	             return strings.HasSuffix(string(env.Subject), ".start") ||
//	                    strings.HasSuffix(string(env.Subject), ".complete")
//	         }),
//	     )
//
// Note on EventFilter.ActorID semantics: the legacy implementation treated
// an empty ActorID on an event as "broadcast — match anyone". The new
// subject-routed API removes this implicit broadcast: subscribe with a
// Pattern that lacks the actor segment, or use WithPredicate explicitly.
type EventFilter struct {
	Types   []EventType
	RunID   string
	NodeID  string
	ActorID string
}

// LegacySubscribeOption configures the LegacyEventBus.Subscribe call.
//
// Deprecated: use SubOption. Removed in v0.2.0.
//
// Migration: replace LegacyWithBufferSize(n) with WithBufferSize(n) on a
// new Bus.Subscribe call; see EventFilter for the pattern/predicate change.
type LegacySubscribeOption func(*subscribeOptions)

type subscribeOptions struct {
	bufferSize int
}

// LegacyWithBufferSize overrides the default channel buffer size for a
// legacy subscription.
//
// Deprecated: use WithBufferSize on the new Bus. Removed in v0.2.0.
//
// Migration:
//
//	old: bus.Subscribe(ctx, filter, event.LegacyWithBufferSize(128))
//	new: bus.Subscribe(ctx, pattern, event.WithBufferSize(128))
func LegacyWithBufferSize(n int) LegacySubscribeOption {
	return func(o *subscribeOptions) {
		if n > 0 {
			o.bufferSize = n
		}
	}
}

// LegacyEventBus is the original interface for publishing and subscribing
// to Event values.
//
// Deprecated: use Bus. Removed in v0.2.0.
//
// Migration: switch the field/parameter type from LegacyEventBus to Bus
// and follow the per-method examples on Event and EventFilter. The two
// interfaces are not interchangeable; this is an intentional break to
// give old call sites a compile-time prompt to migrate.
type LegacyEventBus interface {
	Publish(ctx context.Context, event Event) error
	Subscribe(ctx context.Context, filter EventFilter, opts ...LegacySubscribeOption) (LegacySubscription, error)
	Close() error
}

// LegacySubscription represents an active legacy event subscription.
//
// Deprecated: use Subscription. Removed in v0.2.0.
//
// Migration:
//
//	old: for ev := range sub.Events() { handle(ev) }
//	new: for env := range sub.Events() {            // <-chan Envelope
//	         var p MyPayload
//	         _ = env.Decode(&p)
//	         handle(env, p)
//	     }
type LegacySubscription interface {
	Events() <-chan Event
	Close() error
}

// ---------- LegacyMemoryBus implementation ----------

const defaultBufferSize = 64

// LegacyMemoryBusOption configures a LegacyMemoryBus.
//
// Deprecated: use MemoryBusOption on the new Bus. Removed in v0.2.0.
//
// Migration: replace LegacyWithDropCallback with WithObserver on a new
// MemoryBus; see LegacyWithDropCallback for the call-shape diff.
type LegacyMemoryBusOption func(*LegacyMemoryBus)

// LegacyWithDropCallback sets a callback invoked each time an event is dropped
// because a subscriber's channel buffer is full.
//
// Deprecated: use WithObserver on the new Bus. Removed in v0.2.0.
//
// Known issue (will not be fixed in the legacy implementation): the callback
// is invoked while holding the bus RLock; user callbacks must not call back
// into the bus or block.
//
// Migration:
//
//	old: bus := event.NewLegacyMemoryBus(
//	         event.LegacyWithDropCallback(func(ev event.Event) {
//	             metrics.Inc("dropped", ev.Type)
//	         }),
//	     )
//	new: bus := event.NewMemoryBus(
//	         event.WithObserver(myObserver{}),  // implements OnPublish/OnDeliver/OnDrop
//	     )
//
// The new Observer interface fires outside the bus lock and exposes the
// drop reason (DropReasonBufferFull / DropReasonClosed) and the affected
// SubscriptionID, replacing the lossy single-callback design.
func LegacyWithDropCallback(fn func(Event)) LegacyMemoryBusOption {
	return func(b *LegacyMemoryBus) { b.onDrop = fn }
}

// LegacyMemoryBus is an in-memory LegacyEventBus implementation.
// Thread safety: all methods are safe for concurrent use.
//
// Deprecated: use MemoryBus. Removed in v0.2.0.
//
// Migration:
//
//	old: bus := event.NewLegacyMemoryBus()
//	new: bus := event.NewMemoryBus()
//
// LegacyMemoryBus and MemoryBus are independent types living side by side
// in v0.1.x; pick one per call site rather than mixing both. The legacy
// type retains the original Publish/Subscribe semantics (Event values,
// EventFilter routing, in-lock drop callback) so existing consumers keep
// working unchanged until they migrate.
type LegacyMemoryBus struct {
	mu          sync.RWMutex
	subscribers map[*legacyMemorySub]struct{}
	closed      bool
	dropped     atomic.Int64
	onDrop      func(Event)
}

type legacyMemorySub struct {
	ch     chan Event
	filter EventFilter
	done   chan struct{}
	once   sync.Once
	bus    *LegacyMemoryBus
}

// NewLegacyMemoryBus creates a new in-memory legacy event bus.
//
// Deprecated: use NewMemoryBus. Removed in v0.2.0. See LegacyMemoryBus
// for the side-by-side migration pattern.
func NewLegacyMemoryBus(opts ...LegacyMemoryBusOption) *LegacyMemoryBus {
	b := &LegacyMemoryBus{
		subscribers: make(map[*legacyMemorySub]struct{}),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Dropped returns the total number of events dropped due to full subscriber buffers.
func (b *LegacyMemoryBus) Dropped() int64 {
	return b.dropped.Load()
}

// Publish sends an event to all matching subscribers (non-blocking).
// TraceID and SpanID are automatically extracted from ctx if not already set.
func (b *LegacyMemoryBus) Publish(ctx context.Context, event Event) error {
	if event.ID == "" {
		event.ID = xid.New().String()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.TraceID == "" || event.SpanID == "" {
		if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
			if event.TraceID == "" {
				event.TraceID = sc.TraceID().String()
			}
			if event.SpanID == "" {
				event.SpanID = sc.SpanID().String()
			}
		}
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return fmt.Errorf("event bus closed")
	}

	for sub := range b.subscribers {
		if !sub.matches(event) {
			continue
		}
		select {
		case <-sub.done:
		default:
			select {
			case sub.ch <- event:
			default:
				b.dropped.Add(1)
				if b.onDrop != nil {
					b.onDrop(event)
				}
			}
		}
	}
	return nil
}

// Subscribe creates a new subscription with the given filter. The subscription
// is automatically closed when ctx is cancelled.
func (b *LegacyMemoryBus) Subscribe(ctx context.Context, filter EventFilter, opts ...LegacySubscribeOption) (LegacySubscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, fmt.Errorf("event bus closed")
	}

	o := subscribeOptions{bufferSize: defaultBufferSize}
	for _, fn := range opts {
		fn(&o)
	}

	sub := &legacyMemorySub{
		ch:     make(chan Event, o.bufferSize),
		filter: filter,
		done:   make(chan struct{}),
		bus:    b,
	}
	b.subscribers[sub] = struct{}{}

	go func() {
		select {
		case <-ctx.Done():
			_ = sub.Close()
		case <-sub.done:
		}
	}()

	return sub, nil
}

// Close shuts down the bus and all active subscriptions.
func (b *LegacyMemoryBus) Close() error {
	b.mu.Lock()
	subs := b.subscribers
	b.subscribers = make(map[*legacyMemorySub]struct{})
	b.closed = true
	b.mu.Unlock()

	for sub := range subs {
		sub.closeInternal()
	}
	return nil
}

func (b *LegacyMemoryBus) removeSub(target *legacyMemorySub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, target)
}

func (s *legacyMemorySub) Events() <-chan Event { return s.ch }

func (s *legacyMemorySub) Close() error {
	s.once.Do(func() {
		if s.bus != nil {
			s.bus.removeSub(s)
		}
		close(s.done)
		close(s.ch)
	})
	return nil
}

// closeInternal closes the subscription without removing from bus (used by Bus.Close).
func (s *legacyMemorySub) closeInternal() {
	s.once.Do(func() {
		close(s.done)
		close(s.ch)
	})
}

func (s *legacyMemorySub) matches(event Event) bool {
	if len(s.filter.Types) > 0 {
		found := slices.Contains(s.filter.Types, event.Type)
		if !found {
			return false
		}
	}
	if s.filter.RunID != "" && s.filter.RunID != event.RunID {
		return false
	}
	if s.filter.NodeID != "" && s.filter.NodeID != event.NodeID {
		return false
	}
	if s.filter.ActorID != "" && event.ActorID != "" && s.filter.ActorID != event.ActorID {
		return false
	}
	return true
}

// LegacyNoopBus is a LegacyEventBus that discards all events.
//
// Deprecated: use NoopBus. Removed in v0.2.0.
//
// Migration:
//
//	old: var bus event.LegacyEventBus = event.LegacyNoopBus{}
//	new: var bus event.Bus            = event.NoopBus{}
type LegacyNoopBus struct{}

func (LegacyNoopBus) Publish(context.Context, Event) error { return nil }
func (LegacyNoopBus) Subscribe(context.Context, EventFilter, ...LegacySubscribeOption) (LegacySubscription, error) {
	return &legacyNoopSub{}, nil
}
func (LegacyNoopBus) Close() error { return nil }

var legacyClosedEventCh = func() chan Event {
	ch := make(chan Event)
	close(ch)
	return ch
}()

type legacyNoopSub struct{}

func (s *legacyNoopSub) Events() <-chan Event { return legacyClosedEventCh }
func (s *legacyNoopSub) Close() error         { return nil }
