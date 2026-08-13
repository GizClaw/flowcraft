package runtime

import (
	"errors"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/core/deploy"
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
