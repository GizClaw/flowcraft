package backends

import (
	"context"

	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-bwrap"
)

// Bwrap builds the bubblewrap runner: namespaces and binds confine the command.
func Bwrap() runtime.SandboxFactory {
	return func(_ context.Context, in runtime.SandboxBuild) (sandbox.Runner, error) {
		opts := []bwrap.RunnerOption{bwrap.WithWritablePaths(in.Writes...)}
		if c := in.Config.Bwrap; c != nil {
			if c.Binary != "" {
				opts = append(opts, bwrap.WithBinary(c.Binary))
			}
			if len(c.ExtraFlags) > 0 {
				opts = append(opts, bwrap.WithExtraFlags(c.ExtraFlags...))
			}
			if c.ReadOnlyRoot {
				opts = append(opts, bwrap.WithReadOnlyRoot())
			}
		}
		if in.Journal != nil {
			opts = append(opts, bwrap.WithFileJournalConfig(*in.Journal))
		}
		return bwrap.New(in.Dir, opts...)
	}
}
