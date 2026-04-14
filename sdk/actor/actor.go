// Package actor provides a generic, reusable actor pattern for serial
// message processing with independent context lifecycle.
package actor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrStopped is returned by Send when the actor has been stopped.
var ErrStopped = errors.New("actor stopped")

// ErrAborted is returned when the current execution is aborted via Abort().
var ErrAborted = errors.New("actor aborted")

// Handler processes a single request and returns a response.
type Handler[Req, Resp any] func(ctx context.Context, req Req) (Resp, error)

// Result is the outcome of a single request.
type Result[Resp any] struct {
	Value Resp
	Err   error
}

type message[Req, Resp any] struct {
	req  Req
	done chan Result[Resp]
}

// Option configures an Actor.
type Option func(*config)

type config struct {
	ctx          context.Context
	inboxSize    int
	persistent   bool
	source       string
	drainTimeout time.Duration
}

const (
	defaultInboxSize    = 16
	defaultDrainTimeout = 5 * time.Millisecond
)

func WithPersistent() Option {
	return func(c *config) { c.persistent = true }
}

func WithInboxSize(n int) Option {
	return func(c *config) { c.inboxSize = n }
}

func WithSource(source string) Option {
	return func(c *config) { c.source = source }
}

// WithContext sets a parent context for the actor. When this context is
// cancelled, the actor is cascade-terminated (equivalent to Stop).
func WithContext(ctx context.Context) Option {
	return func(c *config) { c.ctx = ctx }
}

func WithDrainTimeout(d time.Duration) Option {
	return func(c *config) { c.drainTimeout = d }
}

// Actor is a generic execution unit with a serial mailbox and independent
// context lifecycle.
type Actor[Req, Resp any] struct {
	handler      Handler[Req, Resp]
	inbox        chan message[Req, Resp]
	ctx          context.Context    // baseCtx — Stop() cancels this
	cancel       context.CancelFunc // baseCancel
	mu           sync.Mutex
	running      bool
	msgCancel    context.CancelFunc // per-message cancel, nil when idle
	lastActive   time.Time
	persistent   bool
	source       string
	stopped      bool
	stopCh       chan struct{}
	doneCh       chan struct{} // closed when run() goroutine exits
	drainTimeout time.Duration
}

// New creates and starts a new Actor.
func New[Req, Resp any](handler Handler[Req, Resp], opts ...Option) *Actor[Req, Resp] {
	cfg := config{inboxSize: defaultInboxSize}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.drainTimeout <= 0 {
		cfg.drainTimeout = defaultDrainTimeout
	}

	parent := cfg.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	a := &Actor[Req, Resp]{
		handler:      handler,
		inbox:        make(chan message[Req, Resp], cfg.inboxSize),
		ctx:          ctx,
		cancel:       cancel,
		lastActive:   time.Now(),
		persistent:   cfg.persistent,
		source:       cfg.source,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		drainTimeout: cfg.drainTimeout,
	}
	go a.run()
	return a
}

func (a *Actor[Req, Resp]) run() {
	defer close(a.doneCh)
	for {
		select {
		case msg := <-a.inbox:
			msgCtx, msgCancel := context.WithCancel(a.ctx)

			a.mu.Lock()
			a.running = true
			a.msgCancel = msgCancel
			a.lastActive = time.Now()
			a.mu.Unlock()

			value, err := a.safeHandle(msgCtx, msg.req)
			msgCancel()

			a.mu.Lock()
			a.running = false
			a.msgCancel = nil
			a.lastActive = time.Now()
			a.mu.Unlock()

			msg.done <- Result[Resp]{Value: value, Err: err}
		case <-a.stopCh:
			a.drain()
			return
		case <-a.ctx.Done():
			a.doStop()
			a.drain()
			return
		}
	}
}

func (a *Actor[Req, Resp]) safeHandle(ctx context.Context, req Req) (resp Resp, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero Resp
			resp = zero
			err = fmt.Errorf("actor handler panicked: %v", r)
		}
	}()
	return a.handler(ctx, req)
}

// drain empties all pending messages from the inbox, responding with
// ErrStopped. It keeps draining briefly to catch messages that were in
// flight during the stop transition, preventing sender goroutines from
// blocking indefinitely.
func (a *Actor[Req, Resp]) drain() {
	for {
		select {
		case msg := <-a.inbox:
			var zero Resp
			msg.done <- Result[Resp]{Value: zero, Err: ErrStopped}
		case <-time.After(a.drainTimeout):
			return
		}
	}
}

// Send delivers a request to the actor's mailbox and returns a channel that
// receives exactly one Result when processing completes.
func (a *Actor[Req, Resp]) Send(req Req) <-chan Result[Resp] {
	done := make(chan Result[Resp], 1)

	select {
	case <-a.stopCh:
		var zero Resp
		done <- Result[Resp]{Value: zero, Err: ErrStopped}
		return done
	default:
	}

	select {
	case a.inbox <- message[Req, Resp]{req: req, done: done}:
	case <-a.stopCh:
		var zero Resp
		done <- Result[Resp]{Value: zero, Err: ErrStopped}
	}
	return done
}

// TrySend attempts a non-blocking send. It returns the result channel and true
// if the message was enqueued, or nil and false if the mailbox is full or the
// actor is stopped.
func (a *Actor[Req, Resp]) TrySend(req Req) (<-chan Result[Resp], bool) {
	select {
	case <-a.stopCh:
		return nil, false
	default:
	}

	done := make(chan Result[Resp], 1)
	select {
	case a.inbox <- message[Req, Resp]{req: req, done: done}:
		return done, true
	default:
		return nil, false
	}
}

// Context returns the actor's context.
func (a *Actor[Req, Resp]) Context() context.Context { return a.ctx }

// Source returns the creation source tag.
func (a *Actor[Req, Resp]) Source() string { return a.source }

// IsRunning reports whether the actor is currently executing a request.
func (a *Actor[Req, Resp]) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// IsPersistent reports whether the actor is exempt from idle reaping.
func (a *Actor[Req, Resp]) IsPersistent() bool { return a.persistent }

// InboxLen returns the number of pending messages in the inbox.
func (a *Actor[Req, Resp]) InboxLen() int { return len(a.inbox) }

// LastActive returns the time of the last activity.
func (a *Actor[Req, Resp]) LastActive() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastActive
}

// Done returns a channel that is closed when the actor's run loop has exited.
// Use this to wait for the actor to fully quiesce after Stop().
func (a *Actor[Req, Resp]) Done() <-chan struct{} { return a.doneCh }

// Abort cancels the currently running request without stopping the actor.
// Returns true if a running request was cancelled, false if the actor was idle.
// Safe to call concurrently and multiple times.
func (a *Actor[Req, Resp]) Abort() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.msgCancel != nil {
		a.msgCancel()
		a.msgCancel = nil
		return true
	}
	return false
}

// doStop marks the actor as stopped (idempotent, must be called under mu or
// when we know no concurrent doStop/Stop runs).
func (a *Actor[Req, Resp]) doStop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return
	}
	a.stopped = true
	close(a.stopCh)
	a.cancel()
}

// Stop cancels the actor's context and signals the mailbox to reject new
// messages. Safe to call multiple times.
func (a *Actor[Req, Resp]) Stop() {
	a.doStop()
}
