package nsjail

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
	sandboxconfig "github.com/GizClaw/flowcraft/sdk/sandbox/config"
	internalpath "github.com/GizClaw/flowcraft/sdkx/internal/path"
)

// BackendName is the sandbox config backend name for nsjail.
const BackendName = "nsjail"

type settings struct {
	Binary        string   `json:"binary,omitempty"`
	WritablePaths []string `json:"writable_paths,omitempty"`
	ExtraFlags    []string `json:"extra_flags,omitempty"`
}

// Register adds the nsjail backend to a sandbox config builder. Its
// settings are decoded by this package; relative binary and
// writable_paths values resolve beneath the referenced workspace root.
func Register(b *sandboxconfig.Builder) error {
	return b.RegisterFactory(BackendName, func(
		_ context.Context,
		in sandboxconfig.FactoryInput,
	) (coresandbox.Runner, error) {
		var s settings
		if err := in.Settings.Decode(&s); err != nil {
			return nil, errdefs.Validationf(
				"decode nsjail settings: %v", err)
		}
		var options []RunnerOption
		if s.Binary != "" {
			binary, err := internalpath.Resolve(in.Root, s.Binary)
			if err != nil {
				return nil, fmt.Errorf("nsjail settings.binary: %w", err)
			}
			options = append(options, WithBinary(binary))
		}
		writable, err := internalpath.ResolveMany(in.Root, s.WritablePaths)
		if err != nil {
			return nil, fmt.Errorf("nsjail settings.writable_paths: %w", err)
		}
		if writable != nil {
			options = append(options, WithWritablePaths(writable...))
		}
		if s.ExtraFlags != nil {
			options = append(options, WithExtraFlags(s.ExtraFlags...))
		}
		return New(in.Root, options...)
	})
}
