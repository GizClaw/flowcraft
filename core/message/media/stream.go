package media

import (
	"context"
	"io"
	"sync"
)

// Stream is a pull-based source of typed items.
//
// Contract:
//   - Read returns (value, nil) for each available item.
//   - Read returns (zero, io.EOF) when the stream ends normally.
//   - Read returns (zero, err) when the stream is interrupted or the
//     caller's context is canceled.
//   - After returning a non-nil error caused by the stream itself (EOF or
//     interrupt), all subsequent Read calls return the same error.
//
// The item type is instantiated by the owner of the stream: the message
// package exposes Stream[Part] as the canonical live-content transport.
type Stream[T any] interface {
	Read(ctx context.Context) (T, error)
}

// Pipe is the standard implementation of Stream[T]: a typed, bounded pipe
// with explicit normal-close and interrupt semantics.
//
// Producers call Send; it blocks when the buffer is full, which is how
// backpressure propagates from a slow consumer. Consumers call Read.
// Close ends the stream normally (Read drains buffered items, then returns
// io.EOF). Interrupt aborts the stream: Read returns context.Canceled
// immediately, even when buffered items remain.
type Pipe[T any] struct {
	ch        chan T
	interrupt context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	mu      sync.Mutex
	lastErr error
}

// NewPipe creates a buffered Pipe with the given capacity. bufferSize must
// be non-negative.
func NewPipe[T any](bufferSize int) *Pipe[T] {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pipe[T]{
		ch:        make(chan T, bufferSize),
		interrupt: ctx,
		cancel:    cancel,
	}
}

// Read returns the next value from the pipe.
//
//   - Returns io.EOF after Close once all buffered values are consumed.
//   - Returns context.Canceled immediately after Interrupt, skipping
//     buffered values.
//   - Returns ctx.Err() when the caller's context is canceled. This is a
//     per-call cancellation: the stream itself stays usable.
//
// After Read returns a stream-level error (io.EOF or context.Canceled), all
// subsequent Read calls return the same error.
func (p *Pipe[T]) Read(ctx context.Context) (T, error) {
	p.mu.Lock()
	if p.lastErr != nil {
		err := p.lastErr
		p.mu.Unlock()
		var zero T
		return zero, err
	}
	p.mu.Unlock()

	// Interrupt takes precedence over any pending value or a normal Close.
	// Without this pre-check the select below is chosen at random when both
	// a value/closed channel and the interrupt channel are ready, which
	// would let an abnormal termination be observed as a clean io.EOF —
	// e.g. a producer that calls Interrupt() and then runs a deferred
	// Close(). Checking the interrupt channel first makes "Interrupt
	// returns context.Canceled immediately, skipping buffered values" a
	// deterministic guarantee.
	select {
	case <-p.interrupt.Done():
		var zero T
		p.mu.Lock()
		p.lastErr = context.Canceled
		p.mu.Unlock()
		return zero, context.Canceled
	default:
	}

	// The caller's context is checked before values for the same reason:
	// a canceled call should not observe a pending value as if nothing
	// happened. Unlike Interrupt, this is not recorded — a later Read with
	// a fresh context can keep consuming.
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	default:
	}

	select {
	case v, ok := <-p.ch:
		if !ok {
			var zero T
			p.mu.Lock()
			p.lastErr = io.EOF
			p.mu.Unlock()
			return zero, io.EOF
		}
		return v, nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case <-p.interrupt.Done():
		var zero T
		p.mu.Lock()
		p.lastErr = context.Canceled
		p.mu.Unlock()
		return zero, context.Canceled
	}
}

// Send writes a value into the pipe, blocking while the buffer is full
// (backpressure). It returns false when the pipe has been interrupted.
// Callers must not call Send after Close: sending on a closed channel
// panics per Go semantics.
func (p *Pipe[T]) Send(v T) bool {
	select {
	case <-p.interrupt.Done():
		return false
	default:
	}
	select {
	case p.ch <- v:
		return true
	case <-p.interrupt.Done():
		return false
	}
}

// TrySend writes a value into the pipe without blocking. It returns false
// when the pipe is interrupted or the buffer is full.
func (p *Pipe[T]) TrySend(v T) bool {
	select {
	case <-p.interrupt.Done():
		return false
	default:
	}
	select {
	case p.ch <- v:
		return true
	default:
		return false
	}
}

// Close signals normal end of stream. After all buffered values are
// consumed, Read returns io.EOF. Safe to call multiple times (idempotent
// via sync.Once).
func (p *Pipe[T]) Close() {
	p.closeOnce.Do(func() { close(p.ch) })
}

// Interrupt signals abnormal end of stream. Read returns context.Canceled
// immediately, even if buffered values remain. Idempotent.
func (p *Pipe[T]) Interrupt() {
	p.cancel()
}
