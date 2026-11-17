package backends

import (
	"context"

	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
)

// Local builds the in-process runner: it confines nothing beyond what the host
// already is, which is what a deployment declares when the boundary is the host.
func Local() runtime.SandboxFactory {
	return func(_ context.Context, in runtime.SandboxBuild) (sandbox.Runner, error) {
		opts := make([]local.Option, 0, 2)
		if in.Limits.MaxOutputBytes > 0 {
			opts = append(opts, local.WithMaxOutputBytes(in.Limits.MaxOutputBytes))
		}
		if in.Journal != nil {
			opts = append(opts, local.WithFileJournalConfig(*in.Journal))
		}
		return local.New(in.Dir, opts...), nil
	}
}
