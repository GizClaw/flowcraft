package app

import (
	"context"
	"errors"
	"reflect"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

// debugIntegrationFactory captures the deployed memory System into the
// application so the TUI recall panel can query it. The value is
// borrowed; the deployment owns its lifecycle.
type debugIntegrationFactory struct {
	app *App
}

func (f *debugIntegrationFactory) Spec() runtimecore.IntegrationSpec {
	return runtimecore.IntegrationSpec{
		Kind: "forge.debug",
		Deps: []runtimecore.DependencySpec{{
			Name: "memory", Kind: "memory.Assembly",
			Type: reflect.TypeFor[*sdkmemory.Assembly](), Required: true,
		}},
	}
}

func (f *debugIntegrationFactory) Prepare(_ context.Context, _ runtimecore.PrepareInput) (runtimecore.PreparedIntegration, error) {
	return &debugIntegration{app: f.app}, nil
}

type debugIntegration struct {
	app *App
}

func (i *debugIntegration) Bind(_ context.Context, input runtimecore.BindInput) error {
	assembly, err := runtimecore.DependencyAs[sdkmemory.Assembly](input.Dependencies, "memory")
	if err != nil {
		return err
	}
	i.app.memory = assembly
	return nil
}

func (*debugIntegration) DecorateHost(base session.HostFactory) (session.HostFactory, error) {
	if base == nil {
		return nil, errors.New("forge.debug: base host factory is required")
	}
	return base, nil
}

func (*debugIntegration) Start(context.Context) error { return nil }
func (*debugIntegration) Close() error                { return nil }

var (
	_ runtimecore.IntegrationFactory  = (*debugIntegrationFactory)(nil)
	_ runtimecore.PreparedIntegration = (*debugIntegration)(nil)
)
