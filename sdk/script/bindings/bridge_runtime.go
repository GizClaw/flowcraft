package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/script"
)

// RuntimeBinding returns the host object for global "runtime" (e.g. execScript).
// Parent bindings are inherited by sub-scripts.
func RuntimeBinding(ctx context.Context, rt script.Runtime, parentBindings map[string]any) map[string]any {
	return map[string]any{
		"execScript": func(source string, config map[string]any) (*script.Signal, error) {
			env := &script.Env{
				Config:   config,
				Bindings: parentBindings,
			}
			return rt.Exec(ctx, "inline", source, env)
		},
	}
}
