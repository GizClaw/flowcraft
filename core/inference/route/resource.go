package route

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Factory builds an inference.Router resource from a target Assembly
// dep and the full route policy settings.
type Factory struct{}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "inference.Router",
		Impl: "unified",
		Deps: []resource.DepSpec{{
			Name: "target", Type: "inference.Assembly", Required: true,
		}},
	}
}

// New implements resource.Factory.
func (Factory) New(_ context.Context, in resource.Input) (any, error) {
	dep, ok := in.Dep("target")
	if !ok {
		return nil, errdefs.Validationf(
			"inference router: target dep missing")
	}
	target, ok := dep.(*inference.Assembly)
	if !ok {
		return nil, errdefs.Validationf(
			"inference router: target dep is not an *inference.Assembly")
	}
	policy, err := resource.DecodeTyped[Policy](in.Settings)
	if err != nil {
		return nil, err
	}
	if err := policy.ValidateFor(target); err != nil {
		return nil, errdefs.Validationf("inference router: %v", err)
	}
	options, err := policy.Options()
	if err != nil {
		return nil, err
	}
	router, err := New(target, policy.Selectors(target), options...)
	if err != nil {
		return nil, errdefs.Validationf("inference router: %v", err)
	}
	return router, nil
}

// Register adds the router factory to the registry.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}
