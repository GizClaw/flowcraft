package sandbox

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Settings is the strict settings subtree of the local runner factory.
type Settings struct {
	Root string `json:"root"`
}

// Factory builds a local process runner as the sandbox.Runner resource.
type Factory struct{}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{Kind: "sandbox.Runner", Impl: "local"}
}

// New implements resource.Factory.
func (Factory) New(_ context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](in.Settings)
	if err != nil {
		return nil, err
	}
	if settings.Root == "" {
		return nil, errdefs.Validationf(
			"sandbox: settings.root is required")
	}
	return NewLocalRunner(settings.Root), nil
}

// Register adds the local runner factory to the registry.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}
