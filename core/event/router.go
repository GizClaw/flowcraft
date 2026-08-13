package event

import (
	"context"
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
// because its sink returned an error. It is NOT called for Close or
// context cancellation.
func WithOnDetach(fn func(error)) AttachOption {
	return func(o *attachOptions) { o.onDetach = fn }
}

// Router is the generalized subscription fan-out primitive: it
// subscribes patterns on a [Bus] and delivers matching envelopes to
// attached [Sink]s. It is what the agent stream router used to be,
// minus the agent-specific delta decoding — session streaming,
// dashboards, and any multi-consumer subscription build on it.
type Router struct {
	bus Bus

	mu          sync.Mutex
	closed      bool
	attachments map[SubscriptionID]*routerAttachment
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
		bus:         bus,
		attachments: make(map[SubscriptionID]*routerAttachment),
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
	r.mu.Unlock()

	sub, err := r.bus.Subscribe(ctx, pattern, subOpts...)
	if err != nil {
		return nil, err
	}
	a := &routerAttachment{
		router:   r,
		sub:      sub,
		sink:     sink,
		onDetach: o.onDetach,
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = sub.Close()
		return nil, errdefs.NotAvailablef("event router: closed")
	}
	r.attachments[sub.ID()] = a
	r.mu.Unlock()

	r.wg.Add(1)
	go a.run(ctx)
	return func() { r.detach(a) }, nil
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
	attachments := r.attachments
	r.attachments = make(map[SubscriptionID]*routerAttachment)
	r.mu.Unlock()

	for _, a := range attachments {
		_ = a.sub.Close()
	}
	r.wg.Wait()
	return nil
}

func (r *Router) detach(a *routerAttachment) {
	r.mu.Lock()
	delete(r.attachments, a.sub.ID())
	r.mu.Unlock()
	_ = a.sub.Close()
}

type routerAttachment struct {
	router   *Router
	sub      Subscription
	sink     Sink
	onDetach func(error)
}

func (a *routerAttachment) run(ctx context.Context) {
	defer a.router.wg.Done()
	for {
		select {
		case env, ok := <-a.sub.C():
			if !ok {
				a.router.detach(a)
				return
			}
			if err := a.sink.OnEnvelope(ctx, env); err != nil {
				a.router.detach(a)
				if a.onDetach != nil {
					a.onDetach(err)
				}
				return
			}
		case <-ctx.Done():
			a.router.detach(a)
			return
		}
	}
}

var _ Sink = SinkFunc(nil)
