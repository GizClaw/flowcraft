package backends

import (
	"context"

	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-windows"
)

// Windows builds the Windows job-object runner.
func Windows() runtime.SandboxFactory {
	return func(_ context.Context, in runtime.SandboxBuild) (sandbox.Runner, error) {
		opts := []windows.Option{windows.WithWritablePaths(in.Writes...)}
		if c := in.Config.Windows; c != nil && c.WriteConfine {
			opts = append(opts, windows.WithWriteConfinement())
		}
		if in.Journal != nil {
			opts = append(opts, windows.WithFileJournalConfig(*in.Journal))
		}
		return windows.New(in.Dir, opts...)
	}
}
