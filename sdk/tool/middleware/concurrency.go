package middleware

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/message"

	"github.com/GizClaw/flowcraft/sdk/tool"
	"golang.org/x/sync/semaphore"
)

// Concurrency caps how many calls may be in-flight through the chain
// at once; excess callers wait (respecting ctx cancellation) instead
// of overwhelming the tool backends. limit must be positive.
func Concurrency(limit int) tool.Middleware {
	if limit <= 0 {
		panic(fmt.Sprintf("middleware.Concurrency: limit must be positive, got %d", limit))
	}
	sem := semaphore.NewWeighted(int64(limit))
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.Call) message.Result {
			if err := sem.Acquire(ctx, 1); err != nil {
				return message.Result{
					CallID:  call.ID,
					Content: fmt.Sprintf("tool %q failed to acquire execution slot: %v", call.Name, err),
					IsError: true,
				}
			}
			defer sem.Release(1)
			return next(ctx, call)
		}
	}
}
