package knowledge

import (
	"context"
	"sync"
	"time"
)

// ChangeNotifier emits an opaque event whenever the underlying source changes.
//
// Concrete implementations live in adapter packages (e.g.
// sdkx/knowledge/watcher uses fsnotify) so that the sdk core stays
// dependency-free.
//
// Events on the Events channel must be coalesced by the consumer; the
// Reloader below applies a debounce window. Implementations should close
// Events when Close is called.
type ChangeNotifier interface {
	Events() <-chan struct{}
	Close() error
}

// Reloader debounces ChangeNotifier events and triggers Rebuild on a
// stable trailing edge.
//
// Typical use:
//
//	notifier, _ := watcher.NewFSNotifier(store) // sdkx adapter
//	r := knowledge.NewReloader(store, notifier, knowledge.ReloaderOptions{Debounce: 500 * time.Millisecond})
//	go r.Run(ctx)
//
// Rebuild defaults to FSStore.BuildIndex; callers can override to integrate
// with their own RetrievalStore implementations.
type Reloader struct {
	notifier ChangeNotifier
	rebuild  func(ctx context.Context) error
	debounce time.Duration

	mu      sync.Mutex
	pending bool
	wg      sync.WaitGroup
	stop    chan struct{}
}

// ReloaderOptions configures a Reloader.
type ReloaderOptions struct {
	Debounce time.Duration               // default 500ms
	Rebuild  func(context.Context) error // overrides the default rebuild fn
}

// NewReloader wires a ChangeNotifier to a rebuild callback.
//
// When opts.Rebuild is nil and store is non-nil, the reloader falls back to
// store.BuildIndex(ctx).
func NewReloader(store *FSStore, notifier ChangeNotifier, opts ReloaderOptions) *Reloader {
	d := opts.Debounce
	if d <= 0 {
		d = 500 * time.Millisecond
	}
	rebuild := opts.Rebuild
	if rebuild == nil && store != nil {
		rebuild = store.BuildIndex
	}
	return &Reloader{
		notifier: notifier,
		rebuild:  rebuild,
		debounce: d,
		stop:     make(chan struct{}),
	}
}

// Run blocks until Close is called or ctx is cancelled.
func (r *Reloader) Run(ctx context.Context) error {
	if r.notifier == nil || r.rebuild == nil {
		return nil
	}
	var timer *time.Timer
	r.wg.Add(1)
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-r.stop:
			if timer != nil {
				timer.Stop()
			}
			return nil
		case _, ok := <-r.notifier.Events():
			if !ok {
				return nil
			}
			r.mu.Lock()
			if !r.pending {
				r.pending = true
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(r.debounce, func() {
					r.mu.Lock()
					r.pending = false
					r.mu.Unlock()
					rebuildCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()
					_ = r.rebuild(rebuildCtx)
				})
			}
			r.mu.Unlock()
		}
	}
}

// Close stops Run and the underlying ChangeNotifier.
func (r *Reloader) Close() error {
	close(r.stop)
	r.wg.Wait()
	if r.notifier != nil {
		return r.notifier.Close()
	}
	return nil
}

// WorkspaceRoot exposes the underlying workspace root when available.
//
// Returns "" if the workspace does not implement Root().
func (s *FSStore) WorkspaceRoot() string {
	if rw, ok := s.ws.(interface{ Root() string }); ok {
		return rw.Root()
	}
	return ""
}

// Prefix returns the FSStore directory prefix beneath WorkspaceRoot.
//
// Combined: filepath.Join(WorkspaceRoot(), Prefix()) is the on-disk root.
func (s *FSStore) Prefix() string { return s.prefix }
