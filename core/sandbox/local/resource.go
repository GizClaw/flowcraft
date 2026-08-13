package local

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by this
// package.
const ResourceKind = "sandbox.Runner"

// BackendName is the sandbox impl name for the local process backend.
const BackendName = "local"

// Settings is the strict settings subtree of the local runner factory.
type Settings struct {
	Root string `json:"root"`
}

// Factory builds a local process runner as the sandbox.Runner resource.
type Factory struct{}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: BackendName}
}

// New implements resource.Factory.
func (Factory) New(_ context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](in.Settings)
	if err != nil {
		return nil, err
	}
	if settings.Root == "" {
		return nil, errdefs.Validationf(
			"sandbox/local: settings.root is required")
	}
	return New(settings.Root), nil
}

// Register adds the local runner factory to r.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}
