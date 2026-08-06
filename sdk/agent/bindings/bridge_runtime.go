package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type nestedExecSupport interface {
	SupportsNestedExec() bool
}

type nestedExecRuntime interface {
	ExecNested(ctx context.Context, name, source string, env *agent.ScriptEnv) (*agent.ScriptSignal, error)
}

// NewRuntimeBridge returns a late binding for global "runtime".
// It captures env.Bindings so child scripts inherit the final parent bindings.
//
// Script-facing API:
//
//	runtime.execScript(source, config)  // run an inline sub-script;
//	                                    // config becomes the child's
//	                                    // `config` global
//
// Nested execution is a runtime capability, not a guarantee: when the
// underlying runtime cannot honour it (e.g. a pool of size 1 whose
// only VM is busy running the parent), execScript degrades to a
// not_available SIGNAL instead of panicking, so scripts can branch
// on it. It is normally wired by the script node, not user code.
func NewRuntimeBridge(rt agent.ScriptRuntime) LateBindingFunc {
	return func(ctx context.Context, env *agent.ScriptEnv) (string, any) {
		var parentBindings map[string]any
		if env != nil {
			parentBindings = env.Bindings
		}
		return "runtime", RuntimeBinding(ctx, rt, parentBindings)
	}
}

// RuntimeBinding returns the host object for global "runtime" (e.g. execScript).
// Parent bindings are inherited by sub-scripts.
func RuntimeBinding(ctx context.Context, rt agent.ScriptRuntime, parentBindings map[string]any) map[string]any {
	return map[string]any{
		"execScript": func(source string, config map[string]any) (*agent.ScriptSignal, error) {
			env := &agent.ScriptEnv{
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

func nestedNotAvailableSignal(err error) *agent.ScriptSignal {
	msg := "runtime.execScript: nested script execution is not available"
	if err != nil {
		msg = "runtime.execScript: " + err.Error()
	}
	return &agent.ScriptSignal{
		Type:    "error",
		Kind:    string(agent.ErrorKindNotAvailable),
		Message: msg,
	}
}
