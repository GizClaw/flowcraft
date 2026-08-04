// Package memory provides the memory-maintenance Runtime integration.
package memory

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	memoryconfig "github.com/GizClaw/flowcraft/sdkx/memory/config"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	schedulerconfig "github.com/GizClaw/flowcraft/sdkx/scheduler/config"
	memoryscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler/memory"
)

const (
	// Kind is the runtime integration catalog kind.
	Kind = "memory.maintenance"

	memoryDependencyKind = "memory.Assembly"
)

// Factory prepares memory-maintenance integrations.
type Factory struct{}

// NewFactory constructs a memory-maintenance integration factory.
func NewFactory() *Factory { return &Factory{} }

// Spec declares the required memory Assembly and shared scheduler dependencies.
func (*Factory) Spec() runtimecore.IntegrationSpec {
	return runtimecore.IntegrationSpec{
		Kind: Kind,
		Deps: []runtimecore.DependencySpec{
			{
				Name: "memory", Kind: memoryDependencyKind,
				Type: reflect.TypeFor[*memoryconfig.Assembly](), Required: true,
			},
			{
				Name: "scheduler", Kind: schedulerconfig.ResourceKind,
				Type: reflect.TypeFor[sdkscheduler.Server](), Required: true,
			},
		},
	}
}

type emptySettings struct{}

// Prepare rejects every non-empty setting. The Assembly owns all scheduler
// policy through its Lifecycle value.
func (*Factory) Prepare(_ context.Context, input runtimecore.PrepareInput) (runtimecore.PreparedIntegration, error) {
	if _, err := deploy.DecodeSettings[emptySettings](input.Settings.Node()); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory runtime integration %q: settings must be empty: %w", input.Name, err))
	}
	return &integration{name: input.Name}, nil
}

type integration struct {
	name string

	mu        sync.Mutex
	bound     bool
	closed    bool
	scheduler *memoryscheduler.Scheduler

	closeOnce sync.Once
	closeErr  error

	rollbackOnce sync.Once
	rollbackErr  error
	rolledBack   bool
}

// Bind borrows the Assembly and shared Server, then registers maintenance
// rules and a leased worker.
func (i *integration) Bind(ctx context.Context, input runtimecore.BindInput) error {
	if i == nil {
		return errdefs.Validationf("memory runtime integration: nil integration")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return errdefs.NotAvailablef("memory runtime integration %q: integration is closed", i.name)
	}
	if i.bound {
		return errdefs.Conflictf("memory runtime integration %q: already bound", i.name)
	}
	assembly, err := runtimecore.DependencyAs[*memoryconfig.Assembly](input.Dependencies, "memory")
	if err != nil {
		return err
	}
	if assembly.Runtime == nil {
		return errdefs.Validationf("memory runtime integration %q: assembly runtime is nil", i.name)
	}
	server, err := runtimecore.DependencyAs[sdkscheduler.Server](input.Dependencies, "scheduler")
	if err != nil {
		return err
	}
	scheduler, err := memoryscheduler.New(ctx, server, i.name, assembly.Runtime, assembly.Lifecycle)
	if err != nil {
		return fmt.Errorf("memory runtime integration %q: create scheduler: %w", i.name, err)
	}
	i.scheduler = scheduler
	i.bound = true
	return nil
}

// DecorateHost leaves turn Hosts unchanged.
func (*integration) DecorateHost(base session.HostFactory) (session.HostFactory, error) {
	if base == nil {
		return nil, errdefs.Validationf("memory runtime integration: base HostFactory is required")
	}
	return base, nil
}

// Start starts the integration-owned maintenance worker.
func (i *integration) Start(context.Context) error {
	if i == nil {
		return errdefs.NotAvailablef("memory runtime integration: nil integration")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return errdefs.NotAvailablef("memory runtime integration %q: integration is closed", i.name)
	}
	if !i.bound || i.scheduler == nil {
		return errdefs.NotAvailablef("memory runtime integration %q: scheduler is not bound", i.name)
	}
	return i.scheduler.Start()
}

// Close closes only the integration-owned scheduler registration. The borrowed
// Assembly and shared Server remain owned by Runtime and the deployment Result.
func (i *integration) Close() error {
	if i == nil {
		return nil
	}
	i.closeOnce.Do(func() {
		i.mu.Lock()
		i.closed = true
		scheduler := i.scheduler
		rolledBack := i.rolledBack
		i.mu.Unlock()
		if scheduler != nil && !rolledBack {
			i.closeErr = scheduler.Close()
		}
	})
	return i.closeErr
}

// Rollback stops the worker and restores only the lifecycle rules installed
// during Bind. It is used for failed Runtime builds; normal Close deliberately
// preserves the configured rules.
func (i *integration) Rollback(ctx context.Context) error {
	if i == nil {
		return nil
	}
	i.rollbackOnce.Do(func() {
		i.mu.Lock()
		i.closed = true
		scheduler := i.scheduler
		i.mu.Unlock()
		if scheduler != nil {
			i.rollbackErr = scheduler.Rollback(ctx)
		}
		i.mu.Lock()
		i.rolledBack = true
		i.mu.Unlock()
	})
	return i.rollbackErr
}

var (
	_ runtimecore.IntegrationFactory  = (*Factory)(nil)
	_ runtimecore.PreparedIntegration = (*integration)(nil)
	_ runtimecore.BuildRollbacker     = (*integration)(nil)
)
