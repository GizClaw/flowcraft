package kanban

import (
	"context"
	"sync"
)

// watchQueueHint is the initial capacity of a watcher's backlog. The
// queue grows as needed — it is a starting size, not a bound.
const watchQueueHint = 32

// watcher is one live subscription. Its queue is unbounded on purpose:
// a slow consumer must not lose a state transition, because a missed
// terminal event means an executor waits forever.
type watcher struct {
	filter Filter
	out    chan *Card
	notify chan struct{}

	mu     sync.Mutex
	queue  []*Card
	closed bool
}

// Watch returns a channel of cards matching f, delivered in transition
// order. Every state change produces one send, including the initial
// submission.
//
// The channel closes when ctx is cancelled or the board closes. A
// filter of the zero [Filter] observes everything.
//
// Executors conventionally watch for pending work and claim it:
//
//	for card := range k.Watch(ctx, kanban.Filter{Status: kanban.StatusPending}) {
//	    if !k.Claim(card.ID, workerID) {
//	        continue // another worker won the race
//	    }
//	    go run(card)
//	}
//
// A caller that wants to wait for one specific card to finish must
// subscribe BEFORE submitting, or it can miss a fast completion:
//
//	ch := k.Watch(ctx, kanban.Filter{})
//	card, err := k.Submit(ctx, task)
//	for got := range ch {
//	    if got.ID == card.ID && got.Status.IsTerminal() {
//	        return got, nil
//	    }
//	}
func (k *Kanban) Watch(ctx context.Context, f Filter) <-chan *Card {
	if ctx == nil {
		ctx = k.ctx
	}
	w := &watcher{
		filter: f,
		out:    make(chan *Card),
		notify: make(chan struct{}, 1),
		queue:  make([]*Card, 0, watchQueueHint),
	}

	k.wmu.Lock()
	closed := k.isClosed()
	if !closed {
		k.watchers = append(k.watchers, w)
	}
	k.wmu.Unlock()

	if closed {
		close(w.out)
		return w.out
	}

	go w.pump()
	go func() {
		select {
		case <-ctx.Done():
		case <-k.ctx.Done():
		}
		k.removeWatcher(w)
		w.shutdown()
	}()
	return w.out
}

func (k *Kanban) removeWatcher(target *watcher) {
	k.wmu.Lock()
	defer k.wmu.Unlock()
	for i, w := range k.watchers {
		if w == target {
			k.watchers = append(k.watchers[:i], k.watchers[i+1:]...)
			return
		}
	}
}

// notify fans a snapshot out to every matching watcher.
func (k *Kanban) notify(snap *Card) {
	k.wmu.Lock()
	matched := make([]*watcher, 0, len(k.watchers))
	for _, w := range k.watchers {
		if w.filter.matches(snap) {
			matched = append(matched, w)
		}
	}
	k.wmu.Unlock()

	for _, w := range matched {
		w.enqueue(snap)
	}
}

func (w *watcher) enqueue(snap *Card) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.queue = append(w.queue, snap)
	w.mu.Unlock()

	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// pump drains the queue into out, closing out when the watcher shuts
// down and the backlog is exhausted.
func (w *watcher) pump() {
	defer close(w.out)
	for range w.notify {
		for {
			w.mu.Lock()
			if len(w.queue) == 0 {
				closed := w.closed
				w.mu.Unlock()
				if closed {
					return
				}
				break
			}
			next := w.queue[0]
			w.queue = w.queue[1:]
			w.mu.Unlock()
			w.out <- next
		}
	}
	// notify closed: flush whatever is left so a shutting-down watcher
	// still delivers the transitions it already accepted.
	for {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.mu.Unlock()
			return
		}
		next := w.queue[0]
		w.queue = w.queue[1:]
		w.mu.Unlock()
		w.out <- next
	}
}

func (w *watcher) shutdown() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()
	close(w.notify)
}
