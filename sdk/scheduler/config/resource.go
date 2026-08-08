package config

import (
	"context"
	"fmt"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
)

// ResourceKind is the deployment resource kind implemented by
// scheduler servers.
const ResourceKind = "scheduler.Server"

// ServerFactory builds one scheduler server from deployment input.
type ServerFactory = sdkconfig.Func[sdkconfig.Input, sdkscheduler.Server]

// Builder owns an instance-local catalog of server implementations.
type Builder struct {
	servers *sdkconfig.Registry[sdkconfig.Input, sdkscheduler.Server]
}

// NewBuilder returns an empty server catalog.
func NewBuilder() *Builder {
	return &Builder{
		servers: sdkconfig.NewRegistry[sdkconfig.Input, sdkscheduler.Server](),
	}
}

// RegisterFactory adds a server implementation. Empty names, nil
// factories, and duplicates are validation errors.
func (b *Builder) RegisterFactory(impl string, factory ServerFactory) error {
	if b == nil {
		return errdefs.Validationf("scheduler config: builder is nil")
	}
	if err := b.servers.Register(impl, factory); err != nil {
		return errdefs.Validationf("scheduler config: %v", err)
	}
	return nil
}

type deployFactory struct {
	impl    string
	builder *Builder
}

// NewDeployFactory returns the deployment factory for one scheduler
// server implementation. impl is the name used in the deployment
// document's resource entry and must be registered on builder.
func NewDeployFactory(impl string, builder *Builder) sdkconfig.Factory {
	return deployFactory{impl: impl, builder: builder}
}

func (f deployFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: ResourceKind, Impl: f.impl}
}

// New builds one unstarted scheduler server through the registered
// implementation factory. Runtime starts it only after every
// integration has mounted its tasks.
func (f deployFactory) New(ctx context.Context, in sdkconfig.Input) (any, error) {
	if f.builder == nil {
		return nil, errdefs.Validationf(
			"scheduler config: deploy factory builder is nil")
	}
	server, err := f.builder.servers.Build(ctx, f.impl, in)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, errdefs.Validationf(
				"scheduler config: server implementation %q is not registered",
				f.impl)
		}
		return nil, fmt.Errorf("scheduler config: %w", err)
	}
	return server, nil
}
