// Package runtime integrates the flowcraft memory worker into
// sdkx/runtime application lifecycles. Background processing is an
// implementation concern, so the integration lives here instead of in
// the generic adapter layer.
package runtime

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/memory/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

const (
	Kind                 = "memory.worker"
	memoryDependencyKind = "memory.Assembly"
)

// Factory builds the flowcraft memory worker integration.
type Factory struct{}

// NewFactory returns the flowcraft memory worker integration factory.
func NewFactory() *Factory { return &Factory{} }

func (*Factory) Spec() runtimecore.IntegrationSpec {
	return runtimecore.IntegrationSpec{
		Kind: Kind,
		Deps: []runtimecore.DependencySpec{{
			Name: "memory", Kind: memoryDependencyKind,
			Type: reflect.TypeFor[*config.Assembly](), Required: true,
		}},
	}
}

type emptySettings struct{}

func (*Factory) Prepare(_ context.Context, input runtimecore.PrepareInput) (runtimecore.PreparedIntegration, error) {
	var settings emptySettings
	if err := input.Settings.Decode(&settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory runtime integration %q: settings must be empty: %w", input.Name, err,
		))
	}
	return &integration{name: input.Name}, nil
}

type integration struct {
	name string

	mu        sync.Mutex
	assembly  *config.Assembly
	bound     bool
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// Bind borrows the flowcraft Assembly. Ownership remains with the
// deploy Result.
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
	assembly, err := runtimecore.DependencyAs[*config.Assembly](input.Dependencies, "memory")
	if err != nil {
		return err
	}
	if assembly.Runner == nil && assembly.LifecycleRunner == nil {
		return errdefs.Validationf("memory runtime integration %q: assembly has no runner", i.name)
	}
	i.assembly = assembly
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
	if !i.bound || i.assembly == nil {
		i.mu.Unlock()
		return errdefs.NotAvailablef("memory runtime integration %q: assembly is not bound", i.name)
	}
	assembly := i.assembly
	i.mu.Unlock()
	if assembly.Runner != nil {
		if err := assembly.Runner.Start(ctx); err != nil {
			return fmt.Errorf("memory runtime integration %q: start runner: %w", i.name, err)
		}
	}
	if assembly.LifecycleRunner != nil {
		if err := assembly.LifecycleRunner.Start(ctx); err != nil {
			_ = assembly.Close()
			return fmt.Errorf("memory runtime integration %q: start lifecycle runner: %w", i.name, err)
		}
	}
	return nil
}

// Close stops the borrowed runner but never closes the deploy result.
func (i *integration) Close() error {
	if i == nil {
		return nil
	}
	i.closeOnce.Do(func() {
		i.mu.Lock()
		i.closed = true
		assembly := i.assembly
		i.mu.Unlock()
		if assembly != nil {
			i.closeErr = assembly.Close()
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
