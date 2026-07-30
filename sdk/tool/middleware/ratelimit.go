package middleware

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/tool"
)

// RateLimit paces calls per tool according to the tool's declared
// ToolMeta.RateLimit (executions per second). Tools without a
// declaration — or a zero/negative one — pass through unlimited.
//
// The pacer is a strict per-tool schedule: each call takes the next
// slot at 1/rate intervals (no bursting), waiting with ctx
// cancellation respected. The catalog is only read once per tool, on
// first sight; re-registering a tool with different metadata requires
// a new Executor (chains are immutable by design).
func RateLimit(catalog tool.Catalog) tool.Middleware {
	if catalog == nil {
		panic("middleware.RateLimit: catalog is nil")
	}
	limiters := &toolPacers{byTool: make(map[string]*pacer), catalog: catalog}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call tool.Call) tool.Result {
			if p := limiters.forTool(call.Name); p != nil {
				if err := p.wait(ctx); err != nil {
					return tool.Result{
						CallID:  call.ID,
						Content: fmt.Sprintf("tool %q rate-limit wait interrupted: %v", call.Name, err),
						IsError: true,
					}
				}
			}
			return next(ctx, call)
		}
	}
}

// toolPacers lazily resolves per-tool pacers from catalog metadata.
type toolPacers struct {
	mu      sync.Mutex
	byTool  map[string]*pacer
	catalog tool.Catalog
}

// forTool returns the pacer for name, or nil when the tool declares
// no rate limit. The nil-vs-pacer decision is cached per name.
func (t *toolPacers) forTool(name string) *pacer {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.byTool[name]; ok {
		return p
	}
	var p *pacer
	if tl, ok := t.catalog.Get(name); ok {
		if rate := tool.MetadataOf(tl).RateLimit; rate > 0 {
			p = &pacer{interval: time.Duration(float64(time.Second) / rate)}
		}
	}
	t.byTool[name] = p
	return p
}

// pacer hands out strictly spaced start slots for one tool.
type pacer struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

// wait blocks until this call's slot arrives or ctx is done.
func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	slot := p.next
	p.next = slot.Add(p.interval)
	p.mu.Unlock()

	if !slot.After(now) {
		return nil
	}
	timer := time.NewTimer(time.Until(slot))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
