package middleware

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	"go.opentelemetry.io/otel/attribute"
)

// RateLimit paces calls per tool according to the tool's declared
// tool.ToolMeta.RateLimit (executions per second). Tools without a
// declaration — or a zero/negative one — pass through unlimited.
//
// The pacer is a strict per-tool schedule: each call takes the next
// slot at 1/rate intervals (no bursting), waiting with ctx
// cancellation respected. The catalog is only read once per tool, on
// first sight (see [toolMemo]); re-registering a tool with different
// metadata requires a new Executor (chains are immutable by design).
// catalog must be non-nil.
func RateLimit(catalog tool.Catalog) tool.Middleware {
	if catalog == nil {
		panic("middleware.RateLimit: catalog is nil")
	}
	// A nil entry is a cached "this tool declares no rate limit", so the
	// catalog is consulted once per name even for unpaced tools.
	pacers := newToolMemo(func(name string) *pacer {
		t, ok := catalog.Get(name)
		if !ok {
			return nil
		}
		rate := tool.MetadataOf(t).RateLimit
		if rate <= 0 {
			return nil
		}
		return &pacer{interval: time.Duration(float64(time.Second) / rate)}
	})
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, b tool.Binding, call message.ToolCall) message.ToolResult {
			if p := pacers.get(call.Name); p != nil {
				start := time.Now()
				if err := p.wait(ctx); err != nil {
					tool.RecordFailure(ctx, tool.FailureCanceled)
					return message.NewErrorToolResult(call.ID,
						fmt.Sprintf("tool %q rate-limit wait interrupted: %v", call.Name, err))
				}
				// A wait shorter than a millisecond is scheduling noise, not
				// backpressure; recording it would bury the real stalls.
				if waited := time.Since(start); waited >= time.Millisecond {
					annotate(ctx, attribute.Int64(
						telemetry.AttrRateLimitBlockedMs, waited.Milliseconds()))
				}
			}
			return next(ctx, b, call)
		}
	}
}

// toolMemo is a read-mostly name→value cache for what a tool's metadata says.
//
// Every call in the process passes through the lookup and nearly every one is a
// hit, so the entries live in an immutable map published behind an atomic
// pointer: a hit is one atomic load plus a map index and takes no lock at all.
// A miss runs resolve — which is catalog.Get, itself taking the registry lock
// and, for a lazy tool, the proxy's — *before* taking mu, so one slow or
// blocked lookup cannot serialise calls to the other tools; two callers racing
// on the same name both pay for it and then agree on the first value stored.
//
// Every resolved value is cached for the life of the chain, including a value
// that means "nothing to do" (a nil pacer, a not-exempt tool): chains are
// immutable by design, so a tool re-registered with different metadata needs a
// new Executor.
type toolMemo[V any] struct {
	resolve func(name string) V
	// entries is copy-on-write. mu orders the writers; readers never take it.
	entries atomic.Pointer[map[string]V]
	mu      sync.Mutex
}

func newToolMemo[V any](resolve func(name string) V) *toolMemo[V] {
	return &toolMemo[V]{resolve: resolve}
}

// get returns name's value, resolving and caching it on first sight.
func (m *toolMemo[V]) get(name string) V {
	if cached := m.entries.Load(); cached != nil {
		if value, ok := (*cached)[name]; ok {
			return value
		}
	}

	value := m.resolve(name)

	m.mu.Lock()
	defer m.mu.Unlock()
	var current map[string]V
	if cached := m.entries.Load(); cached != nil {
		current = *cached
		if known, ok := current[name]; ok {
			return known
		}
	}
	next := make(map[string]V, len(current)+1)
	for known, cached := range current {
		next[known] = cached
	}
	next[name] = value
	m.entries.Store(&next)
	return value
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
		p.release(slot)
		return ctx.Err()
	}
}

// release returns a withdrawn waiter's slot to the schedule, so a caller that
// gives up does not push every later call one interval further out.
//
// The rollback only happens while the reservation is still the tail: a caller
// that arrived later has already been promised the slot after it, and pulling
// p.next back over that promise would reorder the queue — the later caller
// would wait for a slot that has already passed, and two callers could be
// handed the same slot.
func (p *pacer) release(slot time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next.Equal(slot.Add(p.interval)) {
		p.next = slot
	}
}
