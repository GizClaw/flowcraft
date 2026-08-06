package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

const buildRollbackTimeout = 5 * time.Second

type preparedRecord struct {
	name        string
	integration PreparedIntegration
}

type ownedResources struct {
	manager       *session.Manager
	scheduler     sdkscheduler.Server
	schedulerName string
	router        *agent.StreamRouter
	integrations  []preparedRecord
	result        *deploy.Result
}

func (o *ownedResources) close() error {
	if o == nil {
		return nil
	}
	return closeOwned(o.manager, o.schedulerName, o.router, o.integrations, o.result)
}

func (o *ownedResources) rollback(ctx context.Context) error {
	if o == nil {
		return nil
	}
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	var errs []error
	for _, record := range slices.Backward(o.integrations) {
		if isNil(record.integration) {
			continue
		}
		rollbacker, ok := record.integration.(BuildRollbacker)
		if !ok || isNil(rollbacker) {
			continue
		}
		rollbackCtx, cancel := context.WithTimeout(base, buildRollbackTimeout)
		err := rollbacker.Rollback(rollbackCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"runtime roll back integration %q: %w", record.name, err))
		}
	}
	return errors.Join(errs...)
}

// Runtime owns the complete application object graph built by Builder.
type Runtime struct {
	manager       *session.Manager
	scheduler     sdkscheduler.Server
	schedulerName string
	router        *agent.StreamRouter
	integrations  []preparedRecord
	result        *deploy.Result

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

// Scheduler returns the scheduler control plane, or nil when the runtime was
// built without one. Worker and lifecycle capabilities are intentionally hidden.
func (r *Runtime) Scheduler() sdkscheduler.Control {
	if r == nil || isNil(r.scheduler) {
		return nil
	}
	return &schedulerControlFacade{control: r.scheduler}
}

type schedulerControlFacade struct {
	control sdkscheduler.Control
}

func (f *schedulerControlFacade) PutRule(ctx context.Context, rule sdkscheduler.Rule) error {
	return f.control.PutRule(ctx, rule)
}

func (f *schedulerControlFacade) DeleteRule(ctx context.Context, namespace, id string) error {
	return f.control.DeleteRule(ctx, namespace, id)
}

func (f *schedulerControlFacade) ListRules(
	ctx context.Context,
	namespace string,
) ([]sdkscheduler.Rule, error) {
	return f.control.ListRules(ctx, namespace)
}

func (f *schedulerControlFacade) ScheduleOnce(ctx context.Context, once sdkscheduler.Once) error {
	return f.control.ScheduleOnce(ctx, once)
}

func (f *schedulerControlFacade) CancelOnce(ctx context.Context, namespace, id string) error {
	return f.control.CancelOnce(ctx, namespace, id)
}

// Close stops new session work, waits for active turns, and releases all owned
// objects. Concurrent callers wait for and receive the same aggregate result.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.closeErr = closeOwned(
			r.manager, r.schedulerName, r.router, r.integrations, r.result)
	})
	return r.closeErr
}

func closeOwned(
	manager *session.Manager,
	schedulerName string,
	router *agent.StreamRouter,
	integrations []preparedRecord,
	result *deploy.Result,
) error {
	var errs []error
	if manager != nil {
		if err := manager.Close(); err != nil {
			errs = append(errs, fmt.Errorf("runtime close sessions: %w", err))
		}
	}
	for _, record := range slices.Backward(integrations) {
		if isNil(record.integration) {
			continue
		}
		if err := record.integration.Close(); err != nil {
			errs = append(errs, fmt.Errorf(
				"runtime close integration %q: %w", record.name, err))
		}
	}
	if result != nil && schedulerName != "" {
		if err := result.CloseResource(schedulerName); err != nil {
			errs = append(errs, fmt.Errorf(
				"runtime close scheduler %q: %w", schedulerName, err))
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
