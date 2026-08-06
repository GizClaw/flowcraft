package middleware

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Approver decides whether a gated tool call may proceed. A nil
// error approves; any non-nil error denies — the message is shown to
// the model, so it should be human- and model-readable ("why denied",
// not a stack trace). errdefs.PolicyDenied classifications are
// preserved in the result content.
type Approver interface {
	Approve(ctx context.Context, call message.Call) error
}

// ApproverFunc adapts a plain function to Approver.
type ApproverFunc func(ctx context.Context, call message.Call) error

func (f ApproverFunc) Approve(ctx context.Context, call message.Call) error {
	return f(ctx, call)
}

// Approval gates the named tools behind an Approver. Calls to any
// other tool pass through untouched; a denied call short-circuits
// with an IsError result classified PolicyDenied — it never reaches
// the tool, which matters for tools with side effects.
//
// Place it inside (after) Recover so a misbehaving Approver panic
// still becomes a result, and before Timeout so approval wait time
// does not consume the tool's execution budget.
func Approval(approver Approver, tools ...string) tool.Middleware {
	if approver == nil {
		panic("middleware.Approval: approver is nil")
	}
	gated := make(map[string]struct{}, len(tools))
	for _, name := range tools {
		gated[name] = struct{}{}
	}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.Call) message.Result {
			if _, ok := gated[call.Name]; !ok {
				return next(ctx, call)
			}
			if err := approver.Approve(ctx, call); err != nil {
				return message.Result{
					CallID:  call.ID,
					Content: errdefs.PolicyDeniedf("tool %q call denied: %v", call.Name, err).Error(),
					IsError: true,
				}
			}
			return next(ctx, call)
		}
	}
}
