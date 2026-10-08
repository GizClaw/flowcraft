package craft

import (
	"context"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// Capability is one compile-time extension of a Craft. Optional
// interfaces are discovered with type assertions, so a capability
// implements only what it needs.
type Capability interface {
	// Name identifies the capability in errors and telemetry.
	Name() string
}

// Registrar registers the capability's resource factories.
type Registrar interface {
	Register(*resource.Registry) error
}

// Layers contributes built-in layers on every document composition.
type Layers interface {
	Layers() []deploy.Layer
}

// Schemes contributes ${scheme:...} resolvers. A same-named scheme
// overrides the craft built-ins.
type Schemes interface {
	Resolver() *resource.ReferenceResolver
}

// HostDecorators contributes craft-wide host factory decorators.
type HostDecorators interface {
	HostFactoryDecorators() []runtime.HostFactoryDecorator
}

// ResultHostDecorators contributes craft-wide, deployment-aware host
// factory decorators.
type ResultHostDecorators interface {
	ResultHostFactoryDecorators() []runtime.ResultHostFactoryDecorator
}

// RuntimeBinder binds services that need the runtime handle. It is
// called after every successful runtime build and reload, once per
// runtime key.
type RuntimeBinder interface {
	BindRuntime(ctx context.Context, key RuntimeKey, rt *runtime.Runtime) error
}
