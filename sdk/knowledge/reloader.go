package knowledge

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/internal/background"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	otellog "go.opentelemetry.io/otel/log"
)

// EventNotifier is the producer side of the reload pipeline. Events
// carry dataset/doc granularity so the consumer can issue targeted
// Rebuilds instead of a global one.
//
// Implementations live in adapter packages so the sdk core stays
// dependency-free. Implementations MUST close the Events channel when
// Close() is called.
type EventNotifier interface {
	Events() <-chan ChangeEvent
	Close() error
}

// ReloaderOptions configures EventReloader.
//
//   - Debounce       controls the trailing-edge window.
//   - RebuildTimeout caps each rebuild call.
//   - RetryBackoff   is the initial retry delay after a rebuild error.
//   - MaxRetryBackoff caps exponential retry delay.
type ReloaderOptions struct {
	Debounce        time.Duration
	RebuildTimeout  time.Duration
	RetryBackoff    time.Duration
	MaxRetryBackoff time.Duration
}

// EventReloader debounces ChangeEvents and triggers Rebuild on the
// trailing edge from one owner loop. Rebuilds are serialised: a new
// rebuild waits for the previous one to finish.
//
// Targeted vs global rebuilds: when the debounce window contains
// events for a single (dataset, doc) pair, the rebuild is scoped to
// that pair; when it touches multiple datasets or any EventBulk event,
// a dataset-wide rebuild is issued instead. Mixed datasets in one
// window collapse to a global RebuildScope{} (every dataset).
type EventReloader struct {
	target   Rebuilder
	notifier EventNotifier
	debounce time.Duration
	timeout  time.Duration
	retry    time.Duration
	maxRetry time.Duration

	debouncer *background.Debouncer

	mu        sync.Mutex
	pending   map[scopeKey]struct{} // events accumulated within the window
	stop      chan struct{}
	done      chan struct{}
	started   bool
	stopped   bool
	runCancel context.CancelFunc
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
// RetryBackoff defaults to 200ms and MaxRetryBackoff defaults to 5s.
//
// When target or notifier is nil, Run becomes a no-op (returns
// immediately) and Close is also a no-op for the background loop, so
// callers can wire up unconditionally and the call order does not
// matter.
//
// In the normal (non-nil) configuration the contract is:
//
//   - Run MUST be invoked at most once;
//   - Close blocks until Run has actually exited AND the notifier has
//     been closed, so callers observing a successful Close are
//     guaranteed no further Rebuild can fire. Close also cancels an
//     in-flight rebuild through the Run context.
func NewEventReloader(target Rebuilder, notifier EventNotifier, opts ReloaderOptions) *EventReloader {
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}
	timeout := opts.RebuildTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	retry := opts.RetryBackoff
	if retry <= 0 {
		retry = 200 * time.Millisecond
	}
	maxRetry := opts.MaxRetryBackoff
	if maxRetry <= 0 {
		maxRetry = 5 * time.Second
	}
	if maxRetry < retry {
		maxRetry = retry
	}
	r := &EventReloader{
		target:    target,
		notifier:  notifier,
		debounce:  debounce,
		timeout:   timeout,
		retry:     retry,
		maxRetry:  maxRetry,
		debouncer: background.NewDebouncer(debounce),
		pending:   make(map[scopeKey]struct{}),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
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
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		cancel()
		return errdefs.Validationf("knowledge: EventReloader.Run called more than once")
	}
	if r.stopped {
		r.mu.Unlock()
		cancel()
		return nil
	}
	r.started = true
	r.runCancel = cancel
	r.mu.Unlock()

	defer func() {
		r.debouncer.Stop()
		cancel()
		r.mu.Lock()
		r.runCancel = nil
		r.mu.Unlock()
		close(r.done)
	}()
	return r.loop(runCtx)
}

func (r *EventReloader) loop(ctx context.Context) error {
	var (
		retryTimer *time.Timer
		retryC     <-chan time.Time
		nextRetry  = r.retry
	)
	stopRetry := func() {
		if retryTimer == nil {
			return
		}
		if !retryTimer.Stop() {
			select {
			case <-retryTimer.C:
			default:
			}
		}
		retryTimer = nil
		retryC = nil
	}
	defer stopRetry()

	scheduleRetry := func() {
		if !r.hasPending() {
			return
		}
		if retryTimer != nil {
			retryTimer.Reset(nextRetry)
		} else {
			retryTimer = time.NewTimer(nextRetry)
		}
		retryC = retryTimer.C
		if nextRetry < r.maxRetry {
			nextRetry *= 2
			if nextRetry > r.maxRetry {
				nextRetry = r.maxRetry
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return errdefs.FromContext(ctx.Err())
		case <-r.stop:
			return nil
		case ev, ok := <-r.notifier.Events():
			if !ok {
				return nil
			}
			r.enqueue(ev)
		case <-r.debouncer.C():
			stopRetry()
			if r.rebuildPending(ctx) {
				nextRetry = r.retry
			} else {
				scheduleRetry()
			}
		case <-retryC:
			retryTimer = nil
			retryC = nil
			if r.rebuildPending(ctx) {
				nextRetry = r.retry
			} else {
				scheduleRetry()
			}
		}
	}
}

// Close stops Run and the underlying notifier.
//
// In the normal (non-nil target & notifier) configuration Close blocks
// until Run has actually returned, so on success the caller is
// guaranteed that no further Rebuild call can happen. Calling Close
// before Run is safe; a later Run returns immediately.
//
// When target or notifier is nil, Close is a no-op and may be called
// at any time.
func (r *EventReloader) Close() error {
	if r == nil {
		return nil
	}
	var (
		cancel context.CancelFunc
		wait   bool
	)
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		close(r.stop)
		cancel = r.runCancel
	}
	wait = r.started
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if r.debouncer != nil {
		r.debouncer.Stop()
	}
	var err error
	if r.notifier != nil {
		err = r.notifier.Close()
	}
	if wait {
		<-r.done
	}
	return err
}

// enqueue is the body of the debouncer: it stamps the event into the
// pending set and (re)arms the trailing-edge timer.
func (r *EventReloader) enqueue(ev ChangeEvent) {
	key := scopeKey{datasetID: ev.DatasetID}
	if ev.Kind != EventBulk {
		key.docName = ev.DocName
	}
	r.mu.Lock()
	r.pending[key] = struct{}{}
	r.mu.Unlock()
	r.debouncer.Reset()
}

// rebuildPending executes the rebuild for the accumulated pending set.
//
// Scope reduction:
//   - one (datasetID, docName)            -> RebuildScope{datasetID, docName}
//   - one datasetID, multiple docs/bulk   -> RebuildScope{datasetID}
//   - multiple datasetIDs                 -> RebuildScope{} (everything)
//
// On error, the contributing events remain pending so the owner loop can
// retry with backoff; successful rebuilds remove only the snapshot they
// consumed.
func (r *EventReloader) rebuildPending(ctx context.Context) bool {
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return true
	}
	snapshot := make(map[scopeKey]struct{}, len(r.pending))
	for k := range r.pending {
		snapshot[k] = struct{}{}
	}
	scope := reducePending(snapshot)
	r.mu.Unlock()

	rebuildCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	err := r.target.Rebuild(rebuildCtx, scope)
	if err != nil {
		outcome := background.Classify(err)
		telemetry.Error(ctx, "knowledge reloader rebuild failed",
			otellog.String("outcome", outcome.String()),
			otellog.String("dataset_id", scope.DatasetID),
			otellog.String("doc_name", scope.DocName),
			otellog.String("error", err.Error()))
		return false
	}
	r.mu.Lock()
	for k := range snapshot {
		delete(r.pending, k)
	}
	r.mu.Unlock()
	return true
}

func (r *EventReloader) hasPending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending) > 0
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
