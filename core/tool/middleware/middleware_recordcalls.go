package middleware

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// RecordCalls feeds every dispatched tool call back into the session
// found on the execution context. Without a session on the context the
// middleware is a no-op, so one chain serves runs with and without
// dynamic injection.
func RecordCalls() tool.Middleware {
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			res := next(ctx, call)
			if session, ok := tool.SessionFromContext(ctx); ok {
				session.RecordCall(call)
			}
			return res
		}
	}
}
