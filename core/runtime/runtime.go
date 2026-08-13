package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

// Runtime owns the complete application object graph built by Builder.
type Runtime struct {
	manager *session.Manager
	router  *event.Router
	result  *deploy.Result

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

// Close stops new session work, waits for active turns, and releases
// all owned objects. Concurrent callers wait for and receive the same
// aggregate result.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.closeErr = closeOwned(r.manager, r.router, r.result)
	})
	return r.closeErr
}

func closeOwned(
	manager *session.Manager,
	router *event.Router,
	result *deploy.Result,
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
	if result != nil {
		if err := result.Close(); err != nil {
			errs = append(errs, fmt.Errorf("runtime close deployment: %w", err))
		}
	}
	return errors.Join(errs...)
}
