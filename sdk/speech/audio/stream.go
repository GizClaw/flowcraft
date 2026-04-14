package audio

import (
	"context"
	"io"
	"sync"
)

// Stream is a pull-based data source.
//
// Contract:
//   - Read returns (value, nil) for each available item.
//   - Read returns (zero, io.EOF) when the stream ends normally.
//   - Read returns (zero, err) when the stream is interrupted.
//   - After returning a non-nil error, all subsequent Read calls return the same error.
type Stream[T any] interface {
	Read() (T, error)
}

// Pipe is the standard implementation of Stream[T] — a typed, buffered pipe
// with explicit normal-close and interrupt semantics.
type Pipe[T any] struct {
	ch        chan T
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	mu      sync.Mutex
	lastErr error
}

// NewPipe creates a buffered Pipe with the given capacity.
func NewPipe[T any](bufSize int) *Pipe[T] {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pipe[T]{
		ch:     make(chan T, bufSize),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Read returns the next value from the pipe.
// Returns io.EOF after Close when all buffered values have been consumed.
// Returns context.Canceled immediately after Interrupt, skipping buffered values.
// After returning a non-nil error, all subsequent Read calls return the same error.
func (p *Pipe[T]) Read() (T, error) {
	p.mu.Lock()
	if p.lastErr != nil {
		err := p.lastErr
		p.mu.Unlock()
		var zero T
		return zero, err
	}
	p.mu.Unlock()

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
	case <-p.ctx.Done():
		var zero T
		err := p.ctx.Err()
		p.mu.Lock()
		p.lastErr = err
		p.mu.Unlock()
		return zero, err
	}
}

// Send writes a value into the pipe. Returns false if the pipe has been
// interrupted (context cancelled). Blocks if the buffer is full.
func (p *Pipe[T]) Send(v T) bool {
	select {
	case <-p.ctx.Done():
		return false
	default:
	}
	select {
	case p.ch <- v:
		return true
	case <-p.ctx.Done():
		return false
	}
}

// TrySend writes a value into the pipe without blocking.
// Returns false if the pipe is interrupted or the buffer is full.
func (p *Pipe[T]) TrySend(v T) bool {
	select {
	case <-p.ctx.Done():
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

// Close signals normal end of stream. After all buffered values are consumed,
// Read returns io.EOF. Safe to call multiple times (idempotent via sync.Once).
//
// The caller must ensure no goroutine calls Send after Close, as sending on a
// closed channel panics per Go semantics.
func (p *Pipe[T]) Close() {
	p.closeOnce.Do(func() { close(p.ch) })
}

// Interrupt signals abnormal end of stream. Read returns context.Canceled
// immediately, even if buffered values remain. Idempotent.
func (p *Pipe[T]) Interrupt() {
	p.cancel()
}
