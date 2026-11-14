package middleware

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// RecordCalls feeds every dispatched tool call back into the call recorder
// found on the execution context. Without a recorder the middleware is a no-op,
// so one chain serves runs with and without dynamic injection.
//
// The recorder is the contract seam (tool.CallRecorderFrom): the engine's catalog view
// implements it, and this middleware never imports the engine.
//
// Deprecated: dynamic assemblies already append a built-in view
// recorder when WithDynamic is enabled (see core/tool assembly.go), so
// this middleware is redundant for hosts executing through an assembly.
// Keep it only when constructing a raw tool.Executor without the
// assembly's recorder.
func RecordCalls() tool.Middleware {
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			res := next(ctx, call)
			if recorder, ok := tool.CallRecorderFrom(ctx); ok {
				recorder.RecordCall(call)
			}
			return res
		}
	}
}
