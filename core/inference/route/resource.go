package route

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// depTarget is the assembly this router selects models from.
var depTarget = resource.Require[*inference.Assembly]("target")

// Factory builds an inference.Router resource from a target Assembly
// dep and the full route policy settings.
type Factory struct{}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "inference.Router",
		Impl: "unified",
		Deps: []resource.Requirement{depTarget.Spec()},
	}
}

// New implements resource.Factory.
func (Factory) New(ctx context.Context, in resource.Input) (*Router, error) {
	target, err := depTarget.Get(in)
	if err != nil {
		return nil, err
	}
	policy, err := resource.DecodeTyped[Policy](ctx, in.Settings)
	if err != nil {
		return nil, err
	}
	if err := policy.ValidateFor(target); err != nil {
		return nil, errdefs.Validationf("inference router: %w", err)
	}
	options, err := policy.Options()
	if err != nil {
		return nil, err
	}
	router, err := New(target, policy.Selectors(target), options...)
	if err != nil {
		return nil, errdefs.Validationf("inference router: %w", err)
	}
	return router, nil
}

// Register adds the router factory to the registry.
func Register(r *resource.Registry) error {
	return resource.Register(r, Factory{})
}
