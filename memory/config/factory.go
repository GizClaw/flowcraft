package config

import (
	"context"
	"errors"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"gopkg.in/yaml.v3"
)

// Factory returns the flowcraft memory implementation factory. It is
// registered by applications into the generic sdkx memory assembly,
// mirroring how inference provider factories are registered.
func Factory() sdkmemory.Factory {
	return sdkmemory.FactoryFunc(func(ctx context.Context, input sdkmemory.Input) (sdkmemory.Assembly, error) {
		if input.Workspace == nil {
			return nil, errors.New("memory config: workspace is required")
		}
		if input.Inference == nil {
			return nil, errors.New("memory config: inference runtime is required")
		}
		var settings Settings
		if err := yaml.Unmarshal(input.Settings, &settings); err != nil {
			return nil, err
		}
		builder, err := NewBuilder(input.Workspace, input.Inference)
		if err != nil {
			return nil, err
		}
		built, err := builder.NewAssembly(ctx, settings)
		if err != nil {
			return nil, err
		}
		return built, nil
	})
}
