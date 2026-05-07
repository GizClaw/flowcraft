package vessel

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Handle is the asynchronous receipt for a [Captain.Submit] call.
// Callers wait for the underlying agent.Run to finish via Wait, or
// observe streaming output via the vessel-wide [Logs] helper keyed
// by Handle.RunID.
//
// A single Handle supports MULTIPLE concurrent Wait callers — each
// receives the same (result, err) pair once the run terminates. This
// matters for the daemon: the fleet keeps one bookkeeping goroutine
// Waiting (to release the concurrency gate), while HTTP handlers and
// API endpoints can independently Wait on the same Handle without
// racing for a single-shot value.
type Handle struct {
	// RunID is the engine-level identifier minted (or echoed) when
	// the run started. It is the correlation key for engine
	// envelopes (Subject builders use it) and for any callback
	// machinery the caller wires up.
	RunID string

	// AgentName is the AgentSpec.Name the run was dispatched to.
	AgentName string

	// done is closed by deliver. Wait selects on it to know when
	// the cached result/err pair is safe to read.
	done chan struct{}

	// once guards the single deliver call that publishes the
	// result. A second deliver is a logic bug (the dispatch
	// goroutine should call it exactly once); once+recover keeps
	// us from panicking if it ever happens.
	once sync.Once

	// result + err are written exactly once under once.Do, then
	// read by every Wait caller. No mutex needed: the Wait path
	// only reads after <-done, and done is closed AFTER the writes
	// inside Do — the close acts as the release barrier.
	result *agent.Result
	err    error

	// hookMu guards onTerminate. It is held while appending hooks
	// (OnTerminate) and while draining them (deliver). Because
	// deliver runs the hooks INSIDE the lock and only THEN closes
	// done, any later OnTerminate caller sees terminated == true
	// and synchronously runs the hook itself.
	hookMu      sync.Mutex
	onTerminate []func(*agent.Result, error)
	terminated  bool
}

// newHandle is the internal constructor.
func newHandle(runID, agentName string) *Handle {
	return &Handle{
		RunID:     runID,
		AgentName: agentName,
		done:      make(chan struct{}),
	}
}

// Wait blocks until the run reaches a terminal state, then returns
// the agent.Result and any error returned by agent.Run. Wait honours
// ctx cancellation: when ctx fires before the run finishes, Wait
// returns (nil, ctx.Err()) and the underlying run keeps going (it
// will finish according to its own ctx, which the Captain controls).
//
// Wait is safe to call from multiple goroutines and any number of
// times — every call observes the same final pair once the run
// terminates.
func (h *Handle) Wait(ctx context.Context) (*agent.Result, error) {
	if h == nil {
		return nil, errdefs.Validationf("vessel: nil Handle")
	}
	select {
	case <-h.done:
		return h.result, h.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Done returns the underlying done channel for callers that want to
// integrate Handle completion into a larger select{}. The channel
// is closed when the run terminates; reading it never blocks after
// that point. Result + error are then available via Wait without
// further blocking.
func (h *Handle) Done() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.done
}

// deliver is the producer-side helper. It publishes the result,
// runs every registered OnTerminate hook synchronously (in
// registration order), and only THEN closes the done channel.
// This is the lifecycle-event ordering guarantee that callers
// rely on: when Wait returns or <-Done() unblocks, every hook has
// already executed.
//
// Called exactly once by [Captain.Submit]'s dispatch goroutine;
// the once guard makes a second call a no-op rather than a
// double-close panic. Hooks panicking are isolated by recover —
// one buggy hook MUST NOT block others or leave the run hanging.
func (h *Handle) deliver(res *agent.Result, err error) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.result = res
		h.err = err
		h.hookMu.Lock()
		hooks := h.onTerminate
		h.onTerminate = nil
		h.terminated = true
		h.hookMu.Unlock()
		for _, fn := range hooks {
			runHookSafely(fn, res, err)
		}
		close(h.done)
	})
}

// OnTerminate registers a hook that runs synchronously when the
// underlying run reaches a terminal state, BEFORE Wait returns
// and BEFORE Done is closed. Hooks observe the same (result, err)
// pair that Wait will return. Use this to plug in bookkeeping
// (registry updates, span closure, metrics, checkpoint flush)
// without inventing a separate goroutine that races against the
// done channel.
//
// Contract:
//
//   - Hooks MUST NOT block. They run in the dispatch goroutine
//     and delay every Wait caller by their wall-clock cost.
//   - Hooks MAY panic; the panic is recovered and ignored. Don't
//     rely on it for control flow.
//   - Late registration is safe: if the run has already
//     terminated, OnTerminate invokes fn synchronously with the
//     cached result before returning.
//   - Hooks fire in registration order.
//
// OnTerminate is safe to call from any goroutine and any number
// of times.
func (h *Handle) OnTerminate(fn func(*agent.Result, error)) {
	if h == nil || fn == nil {
		return
	}
	h.hookMu.Lock()
	if !h.terminated {
		h.onTerminate = append(h.onTerminate, fn)
		h.hookMu.Unlock()
		return
	}
	res, err := h.result, h.err
	h.hookMu.Unlock()
	runHookSafely(fn, res, err)
}

// runHookSafely invokes a single hook with panic isolation.
func runHookSafely(fn func(*agent.Result, error), res *agent.Result, err error) {
	defer func() { _ = recover() }()
	fn(res, err)
}
