package middleware

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Concurrency caps how many calls may be in-flight through the chain
// at once; excess callers wait (respecting ctx cancellation) instead
// of overwhelming the tool backends. limit must be positive.
//
// A non-positive limit is reported as an errdefs.Validation error;
// [MustConcurrency] is the panicking form.
func Concurrency(limit int) (tool.Middleware, error) {
	if limit <= 0 {
		return nil, errdefs.Validationf(
			"middleware.Concurrency: limit must be positive, got %d", limit)
	}
	sem := make(chan struct{}, limit)
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return message.NewErrorToolResult(call.ID,
					fmt.Sprintf("tool %q failed to acquire execution slot: %v", call.Name, ctx.Err()))
			}
			return next(ctx, call)
		}
	}, nil
}

// MustConcurrency is [Concurrency] for package-level configuration, where a
// rejected limit is a programmer error: it panics instead of returning the
// validation error.
func MustConcurrency(limit int) tool.Middleware {
	concurrency, err := Concurrency(limit)
	if err != nil {
		panic(err)
	}
	return concurrency
}
