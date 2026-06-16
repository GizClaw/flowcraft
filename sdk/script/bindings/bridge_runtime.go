package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/script"
)

type nestedExecSupport interface {
	SupportsNestedExec() bool
}

type nestedExecRuntime interface {
	ExecNested(ctx context.Context, name, source string, env *script.Env) (*script.Signal, error)
}

// NewRuntimeBridge returns a late binding for global "runtime".
// It captures env.Bindings so child scripts inherit the final parent bindings.
func NewRuntimeBridge(rt script.Runtime) script.LateBindingFunc {
	return func(ctx context.Context, env *script.Env) (string, any) {
		var parentBindings map[string]any
		if env != nil {
			parentBindings = env.Bindings
		}
		return "runtime", RuntimeBinding(ctx, rt, parentBindings)
	}
}

// RuntimeBinding returns the host object for global "runtime" (e.g. execScript).
// Parent bindings are inherited by sub-scripts.
func RuntimeBinding(ctx context.Context, rt script.Runtime, parentBindings map[string]any) map[string]any {
	return map[string]any{
		"execScript": func(source string, config map[string]any) (*script.Signal, error) {
			env := &script.Env{
				Config:   config,
				Bindings: parentBindings,
			}
			if nested, ok := rt.(nestedExecRuntime); ok {
				sig, err := nested.ExecNested(ctx, "inline", source, env)
				if errdefs.IsNotAvailable(err) {
					return nestedNotAvailableSignal(err), nil
				}
				return sig, err
			}
			if support, ok := rt.(nestedExecSupport); ok && !support.SupportsNestedExec() {
				return nestedNotAvailableSignal(nil), nil
			}
			return rt.Exec(ctx, "inline", source, env)
		},
	}
}

func nestedNotAvailableSignal(err error) *script.Signal {
	msg := "runtime.execScript: nested script execution is not available"
	if err != nil {
		msg = "runtime.execScript: " + err.Error()
	}
	return &script.Signal{
		Type:    "error",
		Kind:    string(script.ErrorKindNotAvailable),
		Message: msg,
	}
}
