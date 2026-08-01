package yaml

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

// ResourceKind is the deploy resource category this package builds.
// The value bound is a *config.Assembly, holding the Runtime and the
// optional Router: graph's inference node needs both, and a route
// policy is meaningless once separated from the runtime it validated
// against.
const ResourceKind = "inference.Assembly"

// ResourceSettings is the settings subtree of an inference resource.
type ResourceSettings struct {
	deploy.SubDocument `yaml:",inline"`
}

// NewDeployResource returns a sdkx/deploy resource constructor for
// inference.
//
// Unlike workspace or sandbox, this is a constructor rather than a
// plain ResourceFunc: provider factories and secret resolvers are Go
// values that YAML cannot name. A deployment declares WHICH providers
// it wants and where their credentials live; the host decides which
// driver code and which secret backends exist in the binary. That
// split is what keeps credentials out of the document — profiles carry
// [config.SecretRef], never values.
//
//	b.RegisterResource(yaml.ResourceKind, "yaml",
//	    yaml.NewDeployResource(factories, resolvers))
func NewDeployResource(
	factories map[string]config.Factory,
	resolvers map[string]config.SecretResolver,
) deploy.ResourceFunc {
	return func(ctx context.Context, in deploy.ResourceInput) (any, error) {
		settings, err := deploy.DecodeSettings[ResourceSettings](in.Settings)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"inference config: decode resource settings: %w", err))
		}
		data, err := settings.YAML()
		if err != nil {
			return nil, err
		}
		document, err := decode(data)
		if err != nil {
			return nil, errdefs.Validation(err)
		}
		builder, err := config.NewBuilder(factories, resolvers)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"inference config: %w", err))
		}
		assembly, err := builder.NewAssembly(ctx, document)
		if err != nil {
			return nil, err
		}
		// Return a pointer: Assembly is a two-field value, and every
		// consumer must observe the same Runtime rather than a copy of
		// the struct wrapping it.
		return &assembly, nil
	}
}
