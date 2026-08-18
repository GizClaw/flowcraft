package event

import (
	"context"
	"strconv"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Sink receives one envelope from a Router attachment. Implementations
// must be safe for concurrent use and MUST observe ctx.Done and return
// promptly.
type Sink interface {
	OnEnvelope(ctx context.Context, env Envelope) error
}

// SinkFunc is a func adapter for [Sink].
type SinkFunc func(ctx context.Context, env Envelope) error

// OnEnvelope implements Sink.
func (f SinkFunc) OnEnvelope(ctx context.Context, env Envelope) error {
	return f(ctx, env)
}

// RouterOption tunes a Router.
type RouterOption func(*routerOptions)

// AttachOption tunes one Router.Attach attachment.
type AttachOption func(*attachOptions)

type routerOptions struct {
	attachBackpressure    BackpressurePolicy
	attachBackpressureSet bool
}

// WithDefaultAttachBackpressure sets the backpressure policy for
// attachments that do not override it with [WithAttachBackpressure].
// The bus default (DropNewest) applies when neither the router nor
// the attachment configures one.
func WithDefaultAttachBackpressure(p BackpressurePolicy) RouterOption {
	return func(o *routerOptions) {
		o.attachBackpressure = p
		o.attachBackpressureSet = true
	}
}

type attachOptions struct {
	bufferSize      int
	backpressure    BackpressurePolicy
	backpressureSet bool
	onDetach        func(error)
}

// WithAttachBufferSize sets the subscription buffer for one
// attachment. Values <= 0 fall back to the bus default.
func WithAttachBufferSize(n int) AttachOption {
	return func(o *attachOptions) {
		if n > 0 {
			o.bufferSize = n
		}
	}
}

// WithAttachBackpressure sets the subscription backpressure policy for
// one attachment. Absent, the bus default (DropNewest) applies.
func WithAttachBackpressure(p BackpressurePolicy) AttachOption {
	return func(o *attachOptions) {
		o.backpressure = p
		o.backpressureSet = true
	}
}

// WithOnDetach registers a callback invoked when the attachment stops
// because its sink returned an error. It is NOT called for Close,
// context cancellation, or RemoveBus.
func WithOnDetach(fn func(error)) AttachOption {
	return func(o *attachOptions) { o.onDetach = fn }
}

// Router is the generalized subscription fan-out primitive: it
// subscribes patterns on one or more [Bus]es and delivers matching
// envelopes to attached [Sink]s. It is what the agent stream router
// used to be, minus the agent-specific delta decoding — session
// streaming, dashboards, and any multi-consumer subscription build on
// it.
//
// A Router is multi-bus: the runtime attaches one bus per deployment
// generation, so subscriptions survive generation reloads while each
// generation's events stay on its own bus. [Router.AddBus] and
// [Router.RemoveBus] manage the set; RemoveBus drops the subscriptions
// on that bus without affecting attachments subscribed to other buses.
// Bus implementations must be comparable (use pointer receivers).
type Router struct {
	mu          sync.Mutex
	closed      bool
	buses       map[Bus]struct{}
	attachments map[string]*routerAttachment
	nextID      uint64
	wg          sync.WaitGroup

	attachBackpressure    BackpressurePolicy
	attachBackpressureSet bool
}

// NewRouter constructs a router bound to bus. bus must be non-nil.
func NewRouter(bus Bus, opts ...RouterOption) *Router {
	if bus == nil {
		panic("event.NewRouter: bus is nil")
	}
	r := &Router{
		buses:       map[Bus]struct{}{bus: {}},
		attachments: make(map[string]*routerAttachment),
	}
	routerOpts := routerOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&routerOpts)
		}
	}
	r.attachBackpressure = routerOpts.attachBackpressure
	r.attachBackpressureSet = routerOpts.attachBackpressureSet
	return r
}

// AddBus subscribes every existing attachment to an additional bus.
// Envelopes published on bus are delivered to every attachment that
// was attached before or after the call. AddBus is all-or-nothing: if
// any subscription fails, the bus is not attached and every
// subscription already opened for it is closed. AddBus after Close
// fails with NotAvailable; nil bus is a validation error.
func (r *Router) AddBus(bus Bus) error {
	if r == nil {
		return errdefs.Validationf("event router: AddBus on nil router")
	}
	if bus == nil {
		return errdefs.Validationf("event router: AddBus requires a bus")
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errdefs.NotAvailablef("event router: closed")
	}
	if _, exists := r.buses[bus]; exists {
		r.mu.Unlock()
		return errdefs.Conflictf("event router: bus already attached")
	}
	var subs []*busSub
	for _, a := range r.attachments {
		sub, err := bus.Subscribe(a.ctx, a.pattern, a.subOpts...)
		if err != nil {
			for _, bs := range subs {
				_ = bs.sub.Close()
			}
			r.mu.Unlock()
			return err
		}
		bs := &busSub{attachment: a, bus: bus, sub: sub, ctx: a.ctx}
		a.subs = append(a.subs, bs)
		subs = append(subs, bs)
	}
	r.buses[bus] = struct{}{}
	// Register the goroutines under the lock so a concurrent Close can
	// never observe a zero counter while AddBus is still adding.
	r.wg.Add(len(subs))
	r.mu.Unlock()

	for _, bs := range subs {
		go bs.attachment.runBus(bs)
	}
	return nil
}

// RemoveBus detaches bus from every attachment and unsubscribes it.
// Existing attachments keep their subscriptions on the remaining
// buses. Removing a bus that is not attached is a no-op.
func (r *Router) RemoveBus(bus Bus) error {
	if r == nil || bus == nil {
		return nil
	}
	r.mu.Lock()
	if _, exists := r.buses[bus]; !exists {
		r.mu.Unlock()
		return nil
	}
	delete(r.buses, bus)
	var closed []Subscription
	for _, a := range r.attachments {
		for i := len(a.subs) - 1; i >= 0; i-- {
			if a.subs[i].bus != bus {
				continue
			}
			closed = append(closed, a.subs[i].sub)
			a.subs = append(a.subs[:i], a.subs[i+1:]...)
		}
	}
	r.mu.Unlock()
	for _, sub := range closed {
		_ = sub.Close()
	}
	return nil
}

// busSub is one attachment's subscription on one bus.
type busSub struct {
	attachment *routerAttachment
	bus        Bus
	sub        Subscription
	ctx        context.Context
}

// Attach subscribes pattern and delivers matching envelopes to sink
// until the returned stop function is called, ctx is cancelled, the
// subscription ends, or the sink returns an error (which detaches this
// attachment and reports through WithOnDetach). Attach after Close
// fails with NotAvailable.
func (r *Router) Attach(
	ctx context.Context,
	pattern Pattern,
	sink Sink,
	opts ...AttachOption,
) (func(), error) {
	if sink == nil {
		return nil, errdefs.Validationf("event router: sink is required")
	}
	o := attachOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if !o.backpressureSet && r.attachBackpressureSet {
		o.backpressure = r.attachBackpressure
		o.backpressureSet = true
	}
	var subOpts []SubOption
	if o.bufferSize > 0 {
		subOpts = append(subOpts, WithBufferSize(o.bufferSize))
	}
	if o.backpressureSet {
		subOpts = append(subOpts, WithBackpressure(o.backpressure))
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errdefs.NotAvailablef("event router: closed")
	}
	r.nextID++
	a := &routerAttachment{
		router:   r,
		id:       "attach-" + strconv.FormatUint(r.nextID, 10),
		ctx:      ctx,
		pattern:  pattern,
		subOpts:  subOpts,
		sink:     sink,
		onDetach: o.onDetach,
	}
	r.attachments[a.id] = a
	var subs []*busSub
	for bus := range r.buses {
		sub, err := bus.Subscribe(ctx, pattern, subOpts...)
		if err != nil {
			for _, bs := range subs {
				_ = bs.sub.Close()
			}
			delete(r.attachments, a.id)
			r.mu.Unlock()
			return nil, err
		}
		bs := &busSub{attachment: a, bus: bus, sub: sub, ctx: ctx}
		a.subs = append(a.subs, bs)
		subs = append(subs, bs)
	}
	// Register the goroutines under the lock so a concurrent Close can
	// never observe a zero counter while Attach is still adding.
	r.wg.Add(len(subs))
	r.mu.Unlock()

	for _, bs := range subs {
		go a.runBus(bs)
	}
	return func() { r.detach(a, nil) }, nil
}

// Close tears down every attachment and waits for their loops to
// drain. Close is idempotent.
func (r *Router) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.buses = make(map[Bus]struct{})
	var subs []Subscription
	for _, a := range r.attachments {
		for _, bs := range a.subs {
			subs = append(subs, bs.sub)
		}
		a.subs = nil
	}
	r.attachments = make(map[string]*routerAttachment)
	r.mu.Unlock()

	for _, sub := range subs {
		_ = sub.Close()
	}
	r.wg.Wait()
	return nil
}

// detach removes the attachment from the router, closes every
// remaining bus subscription, and reports cause through onDetach when
// non-nil. Idempotent.
func (r *Router) detach(a *routerAttachment, cause error) {
	a.closeOnce.Do(func() {
		r.mu.Lock()
		delete(r.attachments, a.id)
		subs := a.subs
		a.subs = nil
		r.mu.Unlock()
		for _, bs := range subs {
			_ = bs.sub.Close()
		}
		if cause != nil && a.onDetach != nil {
			a.onDetach(cause)
		}
	})
}

type routerAttachment struct {
	router   *Router
	id       string
	ctx      context.Context
	pattern  Pattern
	subOpts  []SubOption
	sink     Sink
	onDetach func(error)

	subs      []*busSub // guarded by router.mu
	closeOnce sync.Once
}

// runBus delivers envelopes from one bus subscription to the
// attachment's sink. A subscription closed because RemoveBus or Close
// removed it exits quietly; one closed while still attached means the
// bus died and detaches the whole attachment.
func (a *routerAttachment) runBus(bs *busSub) {
	defer a.router.wg.Done()
	for {
		select {
		case env, ok := <-bs.sub.C():
			if !ok {
				a.subEnded(bs)
				return
			}
			if err := a.sink.OnEnvelope(bs.ctx, env); err != nil {
				a.router.detach(a, err)
				return
			}
		case <-bs.ctx.Done():
			a.router.detach(a, nil)
			return
		}
	}
}

// subEnded handles a closed subscription channel. If the bus
// subscription is still attached the bus itself ended (detach); if it
// was removed by RemoveBus/Close the loop just exits.
func (a *routerAttachment) subEnded(bs *busSub) {
	r := a.router
	r.mu.Lock()
	found := false
	for _, sub := range a.subs {
		if sub == bs {
			found = true
			break
		}
	}
	r.mu.Unlock()
	if found {
		r.detach(a, nil)
	}
}
