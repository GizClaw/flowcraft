package middleware

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Concurrency caps how many calls may be in-flight through the chain
// at once; excess callers wait (respecting ctx cancellation) instead
// of overwhelming the tool backends. limit must be positive.
func Concurrency(limit int) tool.Middleware {
	if limit <= 0 {
		panic(fmt.Sprintf("Concurrency: limit must be positive, got %d", limit))
	}
	sem := make(chan struct{}, limit)
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return message.ToolResult{
					CallID:  call.ID,
					Content: fmt.Sprintf("tool %q failed to acquire execution slot: %v", call.Name, ctx.Err()),
					IsError: true,
				}
			}
			return next(ctx, call)
		}
	}
}
