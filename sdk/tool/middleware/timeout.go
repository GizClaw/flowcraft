package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Timeout bounds how long a call may run. The default applies to
// every tool unless overridden per name in perTool:
//
//   - positive value: that tool's own timeout
//   - zero/negative: the tool is exempt (it manages its own deadline)
//
// A default of zero means "no timeout" for tools without an override.
// When the wrapped deadline fires, the result is replaced by a
// timeout IsError result so the model sees an actionable message
// rather than a context stack trace.
func Timeout(defaultTimeout time.Duration, perTool map[string]time.Duration) tool.Middleware {
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call tool.Call) tool.Result {
			limit := defaultTimeout
			if override, ok := perTool[call.Name]; ok {
				limit = override
			}
			if limit <= 0 {
				return next(ctx, call)
			}
			execCtx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()

			res := next(execCtx, call)
			if execCtx.Err() == context.DeadlineExceeded {
				return tool.Result{
					CallID:  call.ID,
					Content: fmt.Sprintf("tool %q timed out after %s", call.Name, limit),
					IsError: true,
				}
			}
			return res
		}
	}
}
