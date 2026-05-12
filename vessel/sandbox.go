package vessel

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// checkpointAttrAgentName is the engine.Checkpoint.Attributes key the
// sandbox host stamps with the dispatching agent's name. Captain.Resume
// reads the same key to route a runID back to the right agent without
// requiring callers to remember the original target. Namespaced under
// "vessel." so engine-level attributes (graph_name, etc.) do not
// collide.
const checkpointAttrAgentName = "vessel.agent_name"

// sandboxHost wraps the caller-supplied [engine.Host] (or a fallback
// engine.NoopHost) so that engine envelopes are forwarded to the
// vessel bus. Every other [engine.Host] capability — Interrupts,
// AskUser, Checkpoint, ReportUsage — delegates to the base via
// embedding.
//
// Concurrency / timeout enforcement happens BEFORE this host is
// invoked (see [admissionGate]); the host itself stays simple and
// re-entrant.
type sandboxHost struct {
	engine.Host
	bus   event.Bus
	store engine.CheckpointStore
}

// newSandboxHost returns the host the Captain hands to every Run.
// base may be nil, in which case engine.NoopHost is substituted so
// every interface method has a working default.
//
// store, when non-nil, intercepts engine.Checkpointer calls and
// persists them. The base host's Checkpoint is still invoked AFTER
// the store call so callers that wired their own Checkpointer (e.g.
// for telemetry) keep observing every checkpoint emission.
func newSandboxHost(base engine.Host, bus event.Bus, store engine.CheckpointStore) engine.Host {
	if base == nil {
		base = engine.NoopHost{}
	}
	return &sandboxHost{Host: base, bus: bus, store: store}
}

// Checkpoint persists via the wired CheckpointStore (if any) before
// delegating to the base host's Checkpointer. Errors from Save are
// returned to the engine — checkpointing is opt-in, so when a store
// IS configured a write failure is meaningful and must surface; the
// engine decides whether to retry / fall back. The base host's
// error is logged via the bus but does not override the store's
// result, because the store is the system-of-record and the base
// host is auxiliary observability.
func (h *sandboxHost) Checkpoint(ctx context.Context, cp engine.Checkpoint) error {
	// Stamp the dispatching agent name on every checkpoint so
	// Captain.Resume can route a runID back to the right agent
	// without forcing callers to remember which agent owned the
	// run. Skip when the ctx has no dispatcher (raw test paths,
	// engine-level usage outside a vessel dispatch).
	if name := dispatcherFromCtx(ctx); name != "" {
		if cp.Attributes == nil {
			cp.Attributes = make(map[string]string, 1)
		}
		// Namespace under "vessel." so engine-level attributes
		// (graph_name, etc.) do not collide. Resume reads the
		// same key.
		if _, exists := cp.Attributes[checkpointAttrAgentName]; !exists {
			cp.Attributes[checkpointAttrAgentName] = name
		}
	}
	if h.store != nil {
		if err := h.store.Save(ctx, cp); err != nil {
			return err
		}
	}
	if h.Host != nil {
		_ = h.Host.Checkpoint(ctx, cp)
	}
	return nil
}

// Publish forwards env to the vessel bus AND to the underlying
// host. The dual fan-out lets callers attach their own
// observability without losing the vessel-internal Logs path.
//
// Errors from either sink are swallowed because envelopes are
// observability, not control flow — the same convention agent.Run /
// kanban / graph follow.
func (h *sandboxHost) Publish(ctx context.Context, env event.Envelope) error {
	if h.bus != nil {
		_ = h.bus.Publish(ctx, env)
	}
	if h.Host != nil {
		_ = h.Host.Publish(ctx, env)
	}
	return nil
}

// ReportUsage debits the per-Run token-budget tracker (if any
// — Captain.Submit installs one on runCtx via budgetCtxKey) and
// then forwards to the base host so caller-supplied UsageReporter
// telemetry (OTel, billing pipelines, …) still observes every
// report.
//
// When the debit pushes the run past spec.Resources.MaxTokensPerTurn
// or the vessel past MaxTokensPerHour, the budget cancels runCtx
// and ReportUsage returns errdefs.RateLimit. Engines that bubble
// the error see the breach immediately; engines that ignore it see
// ctx.Done() at their next iteration. Either way the run cannot
// keep spending past the cap.
func (h *sandboxHost) ReportUsage(ctx context.Context, usage model.TokenUsage) error {
	var budgetErr error
	if ru, ok := ctx.Value(budgetCtxKey{}).(*runUsage); ok && ru != nil {
		// Prefer the explicit Total when reported; fall back to
		// input+output for engines that left it zero. Both legs
		// are what the spec docs commit to.
		delta := usage.TotalTokens
		if delta == 0 {
			delta = usage.InputTokens + usage.OutputTokens
		}
		if err := ru.budget.add(ru, delta); err != nil {
			ru.cancel()
			budgetErr = err
		}
	}
	if h.Host != nil {
		_ = h.Host.ReportUsage(ctx, usage)
	}
	return budgetErr
}

// admissionGate enforces [spec.Resources] caps on the Submit
// path. It is the centralised place where v0.1.0 vessels reject /
// time-out runs that exceed configured budgets — keeping the logic
// here means individual factories / engines / nodes do NOT need to
// know about resource policy.
//
// Two dimensions are enforced:
//
//   - MaxConcurrentRuns: a buffered semaphore. Acquire blocks until
//     a slot frees, but honours ctx (returns errdefs.RateLimit on
//     ctx.Err()). 0 disables the gate.
//   - TurnTimeout: per-Run deadline. The gate returns a derived
//     context.Context to the Captain; the timer cancels the run as
//     if the caller had passed a context.WithTimeout.
type admissionGate struct {
	concurrency chan struct{}
	turnTimeout timeoutFunc
	mu          sync.Mutex
	closed      bool
}

// timeoutFunc is the strategy for adding a per-Run deadline.
// Defined as a function so tests can inject deterministic behaviour
// and so the zero TurnTimeout case stays branch-free at the call
// site.
type timeoutFunc func(parent context.Context) (context.Context, context.CancelFunc)

func newAdmissionGate(maxConcurrent int, turnTimeout timeoutFunc) *admissionGate {
	g := &admissionGate{turnTimeout: turnTimeout}
	if maxConcurrent > 0 {
		g.concurrency = make(chan struct{}, maxConcurrent)
	}
	return g
}

// acquire reserves a concurrency slot (when configured) and applies
// the per-Run timeout. The returned cancel func releases both.
//
// Returns errdefs.RateLimit when the parent ctx fires before a slot
// is available (caller-cancelled while queued). Returns
// errdefs.NotAvailable when the gate has been closed (vessel is
// stopping). Otherwise returns the runCtx + cancel pair the caller
// threads through to agent.Run.
func (g *admissionGate) acquire(parent context.Context) (context.Context, context.CancelFunc, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, nil, errdefs.NotAvailablef("vessel: admission gate closed")
	}
	g.mu.Unlock()

	if g.concurrency != nil {
		select {
		case g.concurrency <- struct{}{}:
		case <-parent.Done():
			return nil, nil, errdefs.RateLimitf("vessel: ctx cancelled while waiting for concurrency slot: %v", parent.Err())
		}
	}

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if g.concurrency != nil {
			select {
			case <-g.concurrency:
			default:
			}
		}
	}

	if g.turnTimeout == nil {
		runCtx, cancel := context.WithCancel(parent)
		return runCtx, func() { cancel(); release() }, nil
	}
	runCtx, cancel := g.turnTimeout(parent)
	return runCtx, func() { cancel(); release() }, nil
}

// close is called from Captain.Stop. After close, [acquire] returns
// errdefs.NotAvailable so any caller racing Stop fails fast instead
// of stalling on a closed concurrency channel.
func (g *admissionGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}
