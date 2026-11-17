package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
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
//
// Tools may also exempt themselves by declaring
// [tool.ToolMeta].SelfTimeout, which requires a catalog to read the
// declaration from; see [TimeoutWithCatalog]. A perTool entry always
// wins over the tool's own claim, so host policy stays authoritative.
func Timeout(defaultTimeout time.Duration, perTool map[string]time.Duration) tool.Middleware {
	return timeout(nil, defaultTimeout, perTool)
}

// TimeoutWithCatalog behaves like [Timeout] but additionally honours
// each tool's [tool.ToolMeta].SelfTimeout declaration, resolved from
// catalog. Tools that bound their own execution — an RPC carrying its
// own transport timeout, for example — are then skipped instead of
// being wrapped in a second, competing deadline.
//
// Metadata is read once per tool, on first sight, matching
// [RateLimit] (see [toolMemo]): chains are immutable by design, so a
// tool re-registered with different metadata needs a new Executor.
// catalog must be non-nil.
func TimeoutWithCatalog(catalog tool.Catalog, defaultTimeout time.Duration, perTool map[string]time.Duration) tool.Middleware {
	if catalog == nil {
		panic("middleware.TimeoutWithCatalog: catalog is nil")
	}
	return timeout(catalog, defaultTimeout, perTool)
}

func timeout(catalog tool.Catalog, defaultTimeout time.Duration, perTool map[string]time.Duration) tool.Middleware {
	// A nil catalog means the claim is unavailable, so every tool is treated as
	// not exempt — the behaviour [Timeout] had before the field existed — and
	// no lookup or cache is involved at all.
	var exempt *toolMemo[bool]
	if catalog != nil {
		exempt = newToolMemo(func(name string) bool {
			t, ok := catalog.Get(name)
			return ok && tool.MetadataOf(t).SelfTimeout
		})
	}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			limit := defaultTimeout
			override, explicit := perTool[call.Name]
			if explicit {
				limit = override
			} else if exempt != nil && exempt.get(call.Name) {
				// The tool says it bounds itself and the host has not
				// overridden that, so impose nothing.
				return next(ctx, call)
			}
			if limit <= 0 {
				return next(ctx, call)
			}
			execCtx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()

			res := next(execCtx, call)
			if execCtx.Err() == context.DeadlineExceeded {
				tool.RecordFailure(ctx, tool.FailureTimeout)
				return message.NewErrorToolResult(call.ID,
					fmt.Sprintf("tool %q timed out after %s", call.Name, limit))
			}
			return res
		}
	}
}
