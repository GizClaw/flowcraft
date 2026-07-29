package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
)

// Reloader reloads a Store snapshot and atomically swaps an inference Runtime
// when the stored revision changes. Factories and secret resolvers run on
// every reload, so credential rotation in the resolver backend takes effect
// without process restarts. When the document declares a route section, each
// swap also rebuilds the deployment Router above the new runtime.
//
// Swap semantics: a swap only replaces the pointers returned by future
// Runtime and Router calls. Runtimes are immutable after construction, so a
// runtime obtained earlier — and every driver, stream, or session opened on
// it — keeps serving with the credentials and models it was built from until
// it becomes unreachable and is collected. Reloads never mutate or invalidate
// in-flight work; rotated credentials apply exclusively to operations
// started through the new runtime. Callers that need eager in-place driver
// eviction within one runtime can use Runtime.Invalidate, but there is no
// cross-runtime teardown: draining old sessions gracefully is the caller's
// responsibility.
//
// Reloader owns no goroutines: call Run or Watch from the caller's
// lifecycle, or drive ReloadOnce directly from an external scheduler.
type Reloader struct {
	builder *Builder
	store   Store
	options []inference.RuntimeOption

	mu       sync.RWMutex
	assembly Assembly
	current  Snapshot
}

// NewReloader builds the initial runtime eagerly so configuration errors fail
// fast at startup instead of surfacing lazily during the first reload.
func NewReloader(
	ctx context.Context,
	builder *Builder,
	store Store,
	options ...inference.RuntimeOption,
) (*Reloader, error) {
	if builder == nil {
		return nil, fmt.Errorf("inference config builder is nil")
	}
	if isNilInterface(store) {
		return nil, fmt.Errorf("inference configuration store is nil")
	}
	reloader := &Reloader{builder: builder, store: store, options: options}
	if err := reloader.ReloadOnce(ctx); err != nil {
		return nil, err
	}
	return reloader, nil
}

// Runtime returns the current runtime. The returned pointer stays valid and
// fully usable after later reloads — runtimes are never mutated in place,
// and sessions opened on an older runtime are unaffected by swaps (see the
// swap semantics documented on Reloader).
func (r *Reloader) Runtime() *inference.Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.assembly.Runtime
}

// Router returns the deployment Router for the current snapshot, or nil when
// the document has no route section. Like Runtime, a returned Router stays
// valid after later reloads: it keeps routing over the runtime it was built
// from, and only future Router calls observe the swap.
func (r *Reloader) Router() *route.Router {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.assembly.Router
}

// Snapshot returns the snapshot backing the current runtime.
func (r *Reloader) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current.Clone()
}

// ReloadOnce reloads the store and swaps the runtime when the revision
// changed. An unchanged revision is a no-op: secrets are not re-resolved, so
// callers that rotate credentials underneath an unchanged document must touch
// the document (or its revision) to force a rebuild.
func (r *Reloader) ReloadOnce(ctx context.Context) error {
	snapshot, err := r.store.Load(ctx)
	if err != nil {
		return err
	}
	r.mu.RLock()
	unchanged := r.assembly.Runtime != nil &&
		r.current.Revision == snapshot.Revision
	r.mu.RUnlock()
	if unchanged {
		return nil
	}
	assembly, err := r.builder.NewAssembly(ctx, snapshot.Document, r.options...)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A concurrent reload may have swapped to a newer revision while we built.
	if r.assembly.Runtime != nil && r.current.Revision == snapshot.Revision {
		return nil
	}
	r.assembly = assembly
	r.current = snapshot.Clone()
	return nil
}

// Run polls ReloadOnce until ctx is cancelled. Poll failures do not stop the
// loop: the last-good runtime keeps serving, and the failure is delivered to
// onError when it is non-nil. The first poll happens immediately; subsequent
// polls respect the interval.
func (r *Reloader) Run(
	ctx context.Context,
	interval time.Duration,
	onError func(error),
) error {
	if interval <= 0 {
		return fmt.Errorf("reload interval must be positive")
	}
	if err := r.reloadOrReport(ctx, onError); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.reloadOrReport(ctx, onError); err != nil {
				return err
			}
		}
	}
}

// Watch reloads on store change signals instead of blind polling. When the
// store implements Notifier, every signal triggers ReloadOnce immediately;
// a slow fallback ticker at fallbackInterval stays armed because signals are
// advisory and may be missed. Stores without Notifier — or a watch that
// cannot be established or dies mid-run — degrade to plain Run polling.
func (r *Reloader) Watch(
	ctx context.Context,
	fallbackInterval time.Duration,
	onError func(error),
) error {
	if fallbackInterval <= 0 {
		return fmt.Errorf("reload fallback interval must be positive")
	}
	notifier, ok := r.store.(Notifier)
	if !ok {
		return r.Run(ctx, fallbackInterval, onError)
	}
	events, err := notifier.Notify(ctx)
	if err != nil {
		if !errors.Is(err, ErrNotifyUnsupported) {
			notify(onError, err)
		}
		return r.Run(ctx, fallbackInterval, onError)
	}
	if err := r.reloadOrReport(ctx, onError); err != nil {
		return err
	}
	ticker := time.NewTicker(fallbackInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, watching := <-events:
			if !watching {
				notify(onError, errors.New(
					"inference configuration watch channel closed",
				))
				return r.Run(ctx, fallbackInterval, onError)
			}
			if err := r.reloadOrReport(ctx, onError); err != nil {
				return err
			}
		case <-ticker.C:
			if err := r.reloadOrReport(ctx, onError); err != nil {
				return err
			}
		}
	}
}

// reloadOrReport reloads once and reports failures. It returns a non-nil
// error only when the caller must stop because the context is done; other
// failures go to onError and the loop continues with the last-good runtime.
func (r *Reloader) reloadOrReport(
	ctx context.Context,
	onError func(error),
) error {
	err := r.ReloadOnce(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	notify(onError, err)
	return nil
}

func notify(onError func(error), err error) {
	if onError != nil {
		onError(err)
	}
}
