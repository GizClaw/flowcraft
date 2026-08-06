package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
)

// Built-in backend names.
const (
	BackendLocal = "local"
)

func (b *Builder) registerBuiltins() {
	if err := b.backends.Register(BackendLocal, buildLocal); err != nil {
		panic(err)
	}
}

type localSettings struct {
	DefaultMaxOutputBytes *int64 `json:"default_max_output_bytes,omitempty"`
}

func buildLocal(_ context.Context, input FactoryInput) (coresandbox.Runner, error) {
	var settings localSettings
	if err := input.Settings.Decode(&settings); err != nil {
		return nil, decodeSettingsError(BackendLocal, err)
	}
	var options []coresandbox.Option
	if settings.DefaultMaxOutputBytes != nil {
		if *settings.DefaultMaxOutputBytes < 0 {
			return nil, errdefs.Validationf(
				"%s settings.default_max_output_bytes must be non-negative",
				BackendLocal)
		}
		options = append(options,
			coresandbox.WithMaxOutputBytes(*settings.DefaultMaxOutputBytes))
	}
	return coresandbox.NewLocalRunner(input.Root, options...), nil
}

func decodeSettingsError(backend string, err error) error {
	return errdefs.Validationf("decode %s settings: %v", backend, err)
}
