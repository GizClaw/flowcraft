package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/script"
)

// BindingFunc creates a named binding for script execution.
// The returned name becomes the global variable name in the script scope,
// and the value is typically a map[string]any of callable Go functions.
type BindingFunc func(ctx context.Context) (name string, value any)

// BuildEnv creates a script.Env from binding funcs evaluated against ctx.
func BuildEnv(ctx context.Context, config map[string]any, fns ...BindingFunc) *script.Env {
	bm := make(map[string]any, len(fns))
	for _, fn := range fns {
		name, val := fn(ctx)
		bm[name] = val
	}
	return &script.Env{Config: config, Bindings: bm}
}
