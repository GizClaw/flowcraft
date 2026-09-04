package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// Runtime owns the complete application object graph built by Builder.
type Runtime struct {
	manager *session.Manager
	router  *event.Router
	result  *deploy.Result
	// registry is the live agent view: dynamically registered agents
	// plus a read-only fallback to the deployment snapshot.
	registry *AgentRegistry
	// liveCatalog is the mutable dynamic tool catalog; nil when the
	// deployment has no dynamic_catalog section.
	liveCatalog *catalogRegistry
	// resources and loader are retained from the Builder so agents can
	// be assembled at runtime with the same factories and source
	// resolution as deployment time.
	resources *resource.Registry
	loader    *resource.Loader
	bus       event.Bus
	// hostDecorator is retained from the Builder so a later Reload can
	// rebuild a host factory for a new deployment generation with the
	// exact same decoration policy.
	hostDecorator HostFactoryDecorator
	// resultHostDecorator is retained from the Builder so a later Reload
	// re-applies the deployment-aware decoration to the new generation's
	// host factory.
	resultHostDecorator ResultHostFactoryDecorator
	// current is the active deployment generation. Reload replaces it
	// atomically; Close retires and closes it.
	current *Generation
	// nextGenID allocates generation ids.
	nextGenID uint64

	// lifecycleMu serializes RegisterAgent / UnregisterAgent / Close so
	// a removal can never sweep away a concurrent registration.
	lifecycleMu sync.Mutex
	closed      bool

	closeOnce sync.Once
	closeErr  error
}

// Sessions returns the runtime-owned transport-neutral session manager.
func (r *Runtime) Sessions() *session.Manager {
	if r == nil {
		return nil
	}
	return r.manager
}

// Attach subscribes pattern on the runtime's borrowed event router and
// delivers matching envelopes to sink until the returned stop function
// is called, ctx is cancelled, the subscription ends, or the sink
// returns an error (which detaches that attachment). It is the
// runtime-level entry point for consumers that want run events without
// resolving the deployment document's event_bus resource themselves —
// for example UI sinks subscribing to prompt lifecycle events:
//
//	detach, err := app.Attach(ctx, session.PatternPromptRequested(), sink)
//	defer detach()
//
// The router is owned by the Runtime: Attach fails with NotAvailable
// after Close, and every attachment is torn down when the Runtime
// closes. External attachments inherit the bus default backpressure
// (DropNewest), so a slow consumer drops envelopes instead of blocking
// the run pipeline; pass event.WithAttachBackpressure to opt into a
// different policy for a specific subscription.
func (r *Runtime) Attach(
	ctx context.Context,
	pattern event.Pattern,
	sink event.Sink,
	opts ...event.AttachOption,
) (func(), error) {
	if r == nil || r.router == nil {
		return nil, errdefs.Validationf("runtime: Attach requires a built Runtime")
	}
	if ctx == nil {
		return nil, errdefs.Validationf("runtime: Attach context is required")
	}
	if sink == nil {
		return nil, errdefs.Validationf("runtime: Attach sink is required")
	}
	return r.router.Attach(ctx, pattern, sink, opts...)
}

// Agent resolves an agent by name from the live view (dynamically
// registered first, then the deployment snapshot).
func (r *Runtime) Agent(name string) (*agent.Agent, bool) {
	if r == nil || r.registry == nil {
		return nil, false
	}
	return r.registry.Agent(name)
}

// AgentNames returns the sorted union of deployed and dynamically
// registered agent names.
func (r *Runtime) AgentNames() []string {
	if r == nil || r.registry == nil {
		return nil
	}
	return r.registry.AgentNames()
}

// Resource borrows the current generation's built deployment resource
// value by deployment name. Like Agent, it resolves through the live
// view: after a Reload it returns the new generation's value, and the
// retired generation's values are closed with it. Callers borrow the
// value and must not close it. For values the application must own the
// lifecycle of (or keep across reloads), construct them outside the
// runtime and inject them through the resource registry instead.
func (r *Runtime) Resource(name string) (any, bool) {
	if r == nil || r.current == nil || r.current.result == nil {
		return nil, false
	}
	return r.current.result.Value(name)
}

// Drain quiesces the runtime for offline replacement. It is serialized
// with RegisterAgent / UnregisterAgent / Reload / Close and delegates
// to the session manager's Drain: new session leases and new Starts on
// already-open leases are refused, while active turns finish naturally
// (bounded by ctx). Drain never interrupts running turns; after it
// returns the runtime stays drained and the caller should Close it once
// its replacement is ready. A drained Runtime cannot serve new work,
// so a timed-out Drain may be retried or followed by Close.
func (r *Runtime) Drain(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if isNilContext(ctx) {
		return errdefs.Validationf("runtime: Drain context is required")
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.closed {
		return errdefs.NotAvailablef("runtime: closed")
	}
	return r.manager.Drain(ctx)
}

// Close stops new session work, waits for active turns, and releases
// all owned objects. Concurrent callers wait for and receive the same
// aggregate result.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.lifecycleMu.Lock()
	r.closed = true
	r.lifecycleMu.Unlock()
	r.closeOnce.Do(func() {
		if r.current != nil {
			r.current.freeze(dynamicInstances(r.registry.Entries()))
		}
		r.closeErr = closeOwned(r.manager, r.router, r.current)
	})
	if r.closeErr != nil {
		telemetry.Error(context.Background(), "runtime close failed",
			otellog.String(telemetry.AttrErrorMessage, r.closeErr.Error()))
	}
	return r.closeErr
}

func closeOwned(
	manager *session.Manager,
	router *event.Router,
	current *Generation,
) error {
	var errs []error
	if manager != nil {
		if err := manager.Close(); err != nil {
			errs = append(errs, fmt.Errorf("runtime close sessions: %w", err))
		}
	}
	if router != nil {
		if err := router.Close(); err != nil {
			errs = append(errs, fmt.Errorf("runtime close stream router: %w", err))
		}
	}
	if current != nil {
		if err := current.close(); err != nil {
			errs = append(errs, fmt.Errorf("runtime close generation: %w", err))
		}
	}
	return errors.Join(errs...)
}
