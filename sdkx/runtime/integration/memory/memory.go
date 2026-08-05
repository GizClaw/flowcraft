// Package memory starts the worker runner owned by a deployed memory Assembly.
package memory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/memory/lifecycle"
	"github.com/GizClaw/flowcraft/memory/worker"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	memoryconfig "github.com/GizClaw/flowcraft/sdkx/memory/config"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

const (
	Kind                 = "memory.worker"
	memoryDependencyKind = "memory.Assembly"
)

type Factory struct{}

func NewFactory() *Factory { return &Factory{} }

func (*Factory) Spec() runtimecore.IntegrationSpec {
	return runtimecore.IntegrationSpec{
		Kind: Kind,
		Deps: []runtimecore.DependencySpec{{
			Name: "memory", Kind: memoryDependencyKind,
			Type: reflect.TypeFor[*memoryconfig.Assembly](), Required: true,
		}},
	}
}

type emptySettings struct{}

func (*Factory) Prepare(_ context.Context, input runtimecore.PrepareInput) (runtimecore.PreparedIntegration, error) {
	if _, err := deploy.DecodeSettings[emptySettings](input.Settings.Node()); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory runtime integration %q: settings must be empty: %w", input.Name, err,
		))
	}
	return &integration{name: input.Name}, nil
}

type integration struct {
	name string

	mu              sync.Mutex
	runner          *worker.Runner
	lifecycleRunner *lifecycle.DreamingRunner
	bound           bool
	closed          bool

	closeOnce sync.Once
	closeErr  error
}

// Bind borrows an Assembly. Ownership remains with the deploy Result.
func (i *integration) Bind(_ context.Context, input runtimecore.BindInput) error {
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
	if assembly.System == nil || assembly.Runner == nil {
		return errdefs.Validationf("memory runtime integration %q: assembly is incomplete", i.name)
	}
	i.runner = assembly.Runner
	i.lifecycleRunner = assembly.LifecycleRunner
	i.bound = true
	return nil
}

func (*integration) DecorateHost(base session.HostFactory) (session.HostFactory, error) {
	if base == nil {
		return nil, errdefs.Validationf("memory runtime integration: base HostFactory is required")
	}
	return base, nil
}

func (i *integration) Start(ctx context.Context) error {
	if i == nil {
		return errdefs.NotAvailablef("memory runtime integration: nil integration")
	}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return errdefs.NotAvailablef("memory runtime integration %q: integration is closed", i.name)
	}
	if !i.bound || i.runner == nil {
		i.mu.Unlock()
		return errdefs.NotAvailablef("memory runtime integration %q: runner is not bound", i.name)
	}
	runner := i.runner
	lifecycleRunner := i.lifecycleRunner
	i.mu.Unlock()
	if err := runner.Start(ctx); err != nil {
		return fmt.Errorf("memory runtime integration %q: start runner: %w", i.name, err)
	}
	if lifecycleRunner != nil {
		if err := lifecycleRunner.Start(ctx); err != nil {
			_ = runner.Close()
			return fmt.Errorf("memory runtime integration %q: start lifecycle runner: %w", i.name, err)
		}
	}
	return nil
}

// Close stops the borrowed runner but never calls Assembly.Close.
func (i *integration) Close() error {
	if i == nil {
		return nil
	}
	i.closeOnce.Do(func() {
		i.mu.Lock()
		i.closed = true
		runner := i.runner
		lifecycleRunner := i.lifecycleRunner
		i.mu.Unlock()
		if runner != nil {
			i.closeErr = runner.Close()
		}
		if lifecycleRunner != nil {
			i.closeErr = errors.Join(i.closeErr, lifecycleRunner.Close())
		}
	})
	return i.closeErr
}

// Rollback performs the same idempotent runner stop for failed Runtime builds.
func (i *integration) Rollback(context.Context) error { return i.Close() }

var (
	_ runtimecore.IntegrationFactory  = (*Factory)(nil)
	_ runtimecore.PreparedIntegration = (*integration)(nil)
	_ runtimecore.BuildRollbacker     = (*integration)(nil)
)
