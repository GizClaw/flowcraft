package knowledge

import (
	"context"
	"sync"
	"time"
)

// EventNotifier is the v0.3.0 producer side of the reload pipeline. It
// supersedes the deprecated ChangeNotifier (defined in deprecated.go):
// events carry dataset/doc granularity so the consumer can issue
// targeted Rebuilds instead of a global one.
//
// Implementations live in adapter packages (e.g. sdkx/knowledge/watcher,
// once it migrates) so the sdk core stays dependency-free.
// Implementations MUST close the Events channel when Close() is called.
type EventNotifier interface {
	Events() <-chan ChangeEvent
	Close() error
}

// ReloaderOptions configures EventReloader (and the deprecated Reloader).
//
// Field set is the union of both consumers:
//   - Debounce       controls the trailing-edge window (both consumers).
//   - RebuildTimeout caps each rebuild call (EventReloader only; the
//     legacy Reloader hard-codes 30s).
//   - Rebuild        is the legacy hook used by the deprecated Reloader to
//     swap the rebuild callback. EventReloader ignores it and always
//     calls Rebuilder.Rebuild on the supplied target.
type ReloaderOptions struct {
	Debounce       time.Duration
	RebuildTimeout time.Duration
	Rebuild        func(context.Context) error
}

// EventReloader debounces ChangeEvents and triggers Rebuild on the
// trailing edge. Rebuilds are serialised: a new rebuild waits for the
// previous one to finish.
//
// Targeted vs global rebuilds: when the debounce window contains
// events for a single (dataset, doc) pair, the rebuild is scoped to
// that pair; when it touches multiple datasets or any EventBulk event,
// a dataset-wide rebuild is issued instead. Mixed datasets in one
// window collapse to a global RebuildScope{} (every dataset).
//
// EventReloader is the v0.3.0 successor to Reloader. The legacy
// Reloader (with its struct{}-channel ChangeNotifier, both in
// deprecated.go) remains exported during the deprecation window and
// will be removed in v0.3.0.
type EventReloader struct {
	target   Rebuilder
	notifier EventNotifier
	debounce time.Duration
	timeout  time.Duration

	mu      sync.Mutex
	pending map[scopeKey]struct{} // events accumulated within the window
	timer   *time.Timer
	wg      sync.WaitGroup
	stop    chan struct{}
	stopped bool
}

// scopeKey collapses a ChangeEvent into a comparable scope so the
// debouncer can dedupe and decide which Rebuild to issue.
type scopeKey struct {
	datasetID string
	docName   string // "" means dataset-wide
}

// NewEventReloader wires a Rebuilder to an EventNotifier.
//
// opts.Debounce defaults to 500ms; opts.RebuildTimeout defaults to 30s.
//
// When target or notifier is nil, Run becomes a no-op (returns
// immediately) and Close is also a no-op for the background loop, so
// callers can wire up unconditionally and the call order does not
// matter.
//
// In the normal (non-nil) configuration the contract is:
//
//   - Run MUST be invoked at most once before Close;
//   - Close blocks until Run has actually exited AND the notifier has
//     been closed, so callers observing a successful Close are
//     guaranteed no further Rebuild / timer callback can fire.
//
// To uphold the second guarantee even when Close races with the
// goroutine that is about to call Run, wg.Add(1) is performed here
// (synchronously, before the constructor returns) rather than inside
// Run. This way wg.Add strictly happens-before any wg.Wait in Close,
// which is what sync.WaitGroup requires when its counter starts at
// zero.
func NewEventReloader(target Rebuilder, notifier EventNotifier, opts ReloaderOptions) *EventReloader {
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}
	timeout := opts.RebuildTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	r := &EventReloader{
		target:   target,
		notifier: notifier,
		debounce: debounce,
		timeout:  timeout,
		pending:  make(map[scopeKey]struct{}),
		stop:     make(chan struct{}),
	}
	if target != nil && notifier != nil {
		r.wg.Add(1)
	}
	return r
}

// Run blocks until ctx is cancelled or Close is called. Run MUST be
// called at most once. When target or notifier is nil, Run returns
// immediately as a no-op.
func (r *EventReloader) Run(ctx context.Context) error {
	if r == nil || r.target == nil || r.notifier == nil {
		return nil
	}
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			r.cancelTimer()
			return ctx.Err()
		case <-r.stop:
			r.cancelTimer()
			return nil
		case ev, ok := <-r.notifier.Events():
			if !ok {
				return nil
			}
			r.enqueue(ctx, ev)
		}
	}
}

// Close stops Run and the underlying notifier.
//
// In the normal (non-nil target & notifier) configuration Close blocks
// until Run has actually returned, so on success the caller is
// guaranteed that no further Rebuild call or timer-driven flush can
// happen. Calling Close before Run was started would deadlock, so
// callers MUST start Run first; this matches the EventReloader
// lifecycle documented on Run / NewEventReloader.
//
// When target or notifier is nil, Close is a no-op and may be called
// at any time.
func (r *EventReloader) Close() error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	close(r.stop)
	r.mu.Unlock()
	r.wg.Wait()
	if r.notifier != nil {
		return r.notifier.Close()
	}
	return nil
}

// enqueue is the body of the debouncer: it stamps the event into the
// pending set and (re)arms the trailing-edge timer.
func (r *EventReloader) enqueue(ctx context.Context, ev ChangeEvent) {
	key := scopeKey{datasetID: ev.DatasetID}
	if ev.Kind != EventBulk {
		key.docName = ev.DocName
	}
	r.mu.Lock()
	r.pending[key] = struct{}{}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(r.debounce, func() { r.flush(ctx) })
	r.mu.Unlock()
}

// flush executes the rebuild for the accumulated pending set.
//
// Scope reduction:
//   - one (datasetID, docName)            -> RebuildScope{datasetID, docName}
//   - one datasetID, multiple docs/bulk   -> RebuildScope{datasetID}
//   - multiple datasetIDs                 -> RebuildScope{} (everything)
func (r *EventReloader) flush(ctx context.Context) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	scope := reducePending(r.pending)
	r.pending = make(map[scopeKey]struct{})
	r.mu.Unlock()

	rebuildCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	_ = r.target.Rebuild(rebuildCtx, scope)
}

func (r *EventReloader) cancelTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

// reducePending collapses the pending set into a single RebuildScope
// per the rules in flush.
func reducePending(set map[scopeKey]struct{}) RebuildScope {
	if len(set) == 0 {
		return RebuildScope{}
	}
	var (
		datasets = make(map[string]struct{}, len(set))
		docs     = make(map[string]struct{}, len(set))
		anyBulk  bool
	)
	for k := range set {
		datasets[k.datasetID] = struct{}{}
		if k.docName == "" {
			anyBulk = true
		} else {
			docs[k.docName] = struct{}{}
		}
	}
	if len(datasets) > 1 {
		return RebuildScope{}
	}
	var ds string
	for k := range datasets {
		ds = k
	}
	if anyBulk || len(docs) > 1 {
		return RebuildScope{DatasetID: ds}
	}
	if len(docs) == 1 {
		var doc string
		for d := range docs {
			doc = d
		}
		return RebuildScope{DatasetID: ds, DocName: doc}
	}
	return RebuildScope{DatasetID: ds}
}
