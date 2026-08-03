package kanban

import (
	"context"
	"slices"
	"sync"
)

const watchQueueHint = 32

type watcher struct {
	filter Filter
	out    chan *Card
	notify chan struct{}
	done   chan struct{}

	mu     sync.Mutex
	queue  []*Card
	closed bool
}

// Watch streams matching delegation card transitions until ctx is canceled or
// the backend closes. It observes future transitions and does not replay.
func (b *Board) Watch(ctx context.Context, filter Filter) <-chan *Card {
	if ctx == nil {
		ctx = b.ctx
	}
	watcher := &watcher{
		filter: filter,
		out:    make(chan *Card),
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
		queue:  make([]*Card, 0, watchQueueHint),
	}

	b.wmu.Lock()
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if !closed {
		b.watchers = append(b.watchers, watcher)
	}
	b.wmu.Unlock()
	if closed {
		close(watcher.out)
		return watcher.out
	}

	go watcher.pump()
	go func() {
		select {
		case <-ctx.Done():
		case <-b.ctx.Done():
		}
		b.removeWatcher(watcher)
		watcher.shutdown()
	}()
	return watcher.out
}

func (b *Board) removeWatcher(target *watcher) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	for index, watcher := range b.watchers {
		if watcher == target {
			b.watchers = slices.Delete(b.watchers, index, index+1)
			return
		}
	}
}

func (b *Board) notify(snapshot *Card) {
	b.wmu.Lock()
	matched := make([]*watcher, 0, len(b.watchers))
	for _, watcher := range b.watchers {
		if watcher.filter.matches(snapshot) {
			matched = append(matched, watcher)
		}
	}
	b.wmu.Unlock()
	for _, watcher := range matched {
		watcher.enqueue(snapshot)
	}
}

func (w *watcher) enqueue(snapshot *Card) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.queue = append(w.queue, snapshot)
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *watcher) pump() {
	defer close(w.out)
	for {
		w.mu.Lock()
		if len(w.queue) > 0 {
			next := w.queue[0]
			w.queue = w.queue[1:]
			w.mu.Unlock()
			select {
			case w.out <- next:
			case <-w.done:
				return
			}
			continue
		}
		closed := w.closed
		w.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-w.notify:
		case <-w.done:
			return
		}
	}
}

func (w *watcher) shutdown() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.done)
	w.mu.Unlock()
}
