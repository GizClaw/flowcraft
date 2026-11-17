package runtime

import (
	"context"

	"github.com/GizClaw/flowcraft/core/config"
)

// LoadLayers turns a stack of configuration layers into a validated Config.
//
// The merge, the placeholder expansion and the strict decoding belong to
// [config]; what stays here is the one thing this package owns: a Config that
// reached a caller through this function has passed [Config.Validate]. A caller
// that also wants the merged document — its canonical bytes, the layer that
// supplied a value — calls config.Load directly and validates the result
// itself.
func LoadLayers(ctx context.Context, layers []config.Layer, opts ...config.Option) (Config, error) {
	settings := append([]config.Option{
		config.WithLabel("runtime"),
		// A runtime is built from the value; nothing here reports which layer
		// supplied a field, so the merge keeps no origins and a 256-key stack
		// costs one walk less.
		config.WithoutProvenance(),
	}, opts...)
	cfg, _, err := config.Load[Config](ctx, layers, config.Strict[Config], settings...)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
