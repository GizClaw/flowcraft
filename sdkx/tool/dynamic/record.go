package dynamic

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
)

// RecordCalls feeds every dispatched tool call back into the session
// catalog found on the execution context, through the optional
// RecordCall contract. It records attempts (including denied or failed
// calls): the model choosing a tool is itself the Selected/UsedRecently
// signal. Without a catalog on the context the middleware is a no-op,
// so one chain serves sessions with and without dynamic injection.
func RecordCalls() sdktool.Middleware {
	return func(next sdktool.Dispatch) sdktool.Dispatch {
		return func(ctx context.Context, call message.Call) message.Result {
			res := next(ctx, call)
			if catalog, ok := sdktool.CatalogFromContext(ctx); ok {
				if recorder, ok := catalog.(interface{ RecordCall(message.Call) }); ok {
					recorder.RecordCall(call)
				}
			}
			return res
		}
	}
}
