package middleware

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Recover converts a panicking tool (or inner middleware) into an
// IsError result instead of crashing the caller's goroutine. Put it
// first in the chain: without it a panic inside ExecuteAll escapes
// the fan-out goroutine and takes the process down.
func Recover() tool.Middleware {
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call tool.Call) (res tool.Result) {
			defer func() {
				if rv := recover(); rv != nil {
					res = tool.Result{
						CallID:  call.ID,
						Content: fmt.Sprintf("tool %q panicked: %v", call.Name, rv),
						IsError: true,
					}
				}
			}()
			return next(ctx, call)
		}
	}
}
