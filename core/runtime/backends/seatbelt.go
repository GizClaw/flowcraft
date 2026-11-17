package backends

import (
	"context"

	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-seatbelt"
)

// Seatbelt builds the macOS sandbox profile runner.
func Seatbelt() runtime.SandboxFactory {
	return func(_ context.Context, in runtime.SandboxBuild) (sandbox.Runner, error) {
		opts := []seatbelt.RunnerOption{seatbelt.WithWritablePaths(in.Writes...)}
		if c := in.Config.Seatbelt; c != nil {
			if c.Binary != "" {
				opts = append(opts, seatbelt.WithBinary(c.Binary))
			}
			if c.ReadOnlyRoot {
				opts = append(opts, seatbelt.WithReadOnlyRoot())
			}
		}
		if in.Journal != nil {
			opts = append(opts, seatbelt.WithFileJournalConfig(*in.Journal))
		}
		return seatbelt.New(in.Dir, opts...)
	}
}
