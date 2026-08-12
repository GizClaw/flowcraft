package middleware

import (
	"context"
	"fmt"
	"sync"
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
// [RateLimit]: chains are immutable by design, so a tool re-registered
// with different metadata needs a new Executor.
func TimeoutWithCatalog(catalog tool.Catalog, defaultTimeout time.Duration, perTool map[string]time.Duration) tool.Middleware {
	if catalog == nil {
		panic("middleware.TimeoutWithCatalog: catalog is nil")
	}
	return timeout(catalog, defaultTimeout, perTool)
}

func timeout(catalog tool.Catalog, defaultTimeout time.Duration, perTool map[string]time.Duration) tool.Middleware {
	exempt := &selfTimeoutCache{catalog: catalog}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			limit := defaultTimeout
			override, explicit := perTool[call.Name]
			if explicit {
				limit = override
			} else if exempt.forTool(call.Name) {
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
				return message.ToolResult{
					CallID:  call.ID,
					Content: fmt.Sprintf("tool %q timed out after %s", call.Name, limit),
					IsError: true,
				}
			}
			return res
		}
	}
}

// selfTimeoutCache resolves and memoizes each tool's SelfTimeout
// claim. A nil catalog means the claim is unavailable, so every tool is
// treated as not exempt — the behaviour [Timeout] had before the field
// existed.
type selfTimeoutCache struct {
	mu      sync.Mutex
	byTool  map[string]bool
	catalog tool.Catalog
}

func (c *selfTimeoutCache) forTool(name string) bool {
	if c.catalog == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if exempt, ok := c.byTool[name]; ok {
		return exempt
	}
	exempt := false
	if t, ok := c.catalog.Get(name); ok {
		exempt = tool.MetadataOf(t).SelfTimeout
	}
	if c.byTool == nil {
		c.byTool = make(map[string]bool)
	}
	c.byTool[name] = exempt
	return exempt
}
