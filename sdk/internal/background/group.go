package background

import (
	"context"
	"sync"
)

// Group owns a lifecycle-bound context and a set of goroutines derived from it.
//
// Close is idempotent. It cancels the group context, waits for every accepted
// goroutine to return, and then closes Done. Start synchronizes wg.Add before
// launching the goroutine, so callers cannot race Close's Wait with a late Add.
type Group struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closing bool

	closeOnce sync.Once
	done      chan struct{}
}

// NewGroup returns a fresh lifecycle group. A nil parent is treated as
// context.Background().
func NewGroup(parent context.Context) *Group {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Group{
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// Context returns the owner context shared by all accepted work.
func (g *Group) Context() context.Context {
	if g == nil {
		return context.Background()
	}
	return g.ctx
}

// Done is closed after Close has canceled the context and all accepted work has
// returned.
func (g *Group) Done() <-chan struct{} {
	if g == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return g.done
}

// Start launches fn in a goroutine bound to the group context. It returns false
// when Close has already started or fn is nil.
func (g *Group) Start(fn func(context.Context)) bool {
	if g == nil || fn == nil {
		return false
	}
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return false
	}
	g.wg.Add(1)
	ctx := g.ctx
	g.mu.Unlock()

	go func() {
		defer g.wg.Done()
		fn(ctx)
	}()
	return true
}

// Close cancels the group context and waits for all accepted work to drain.
func (g *Group) Close() {
	if g == nil {
		return
	}
	g.closeOnce.Do(func() {
		g.mu.Lock()
		g.closing = true
		g.cancel()
		g.mu.Unlock()

		g.wg.Wait()
		close(g.done)
	})
	<-g.done
}
