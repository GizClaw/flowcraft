package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// Run executes one turn of ag against eng with the given req.
//
// Run is intentionally minimalist: it owns identifier minting, board
// assembly, observer dispatch, and result classification — and
// nothing else. Anything that looks like "policy" (load conversation
// history, run RAG retrieval, write transcripts after a turn, emit
// metrics, route engine envelopes to a bus, accumulate token usage,
// …) lives outside Run on:
//
//   - [Observer] / [Decider] for lifecycle hooks;
//   - [BoardSeeder] for engine-input shaping;
//   - the caller-supplied [engine.Host] (see [WithEngineHost]) for
//     every host-side capability the engine needs (event publishing,
//     interrupt injection, user prompting, checkpoint persistence,
//     usage reporting). When omitted, agent falls back to
//     [engine.NoopHost] — which is fine for trivial / test runs but
//     gives up every observability and HITL capability.
//
// # Wiring sequence
//
//  1. Mint a RunID (req.RunID wins, else autogenerate).
//  2. Build an [engine.Board] using either a caller-supplied
//     BoardSeeder ([WithBoardSeed]) or the default seeder, which
//     simply appends req.Message to MainChannel and copies
//     req.Inputs to board vars.
//  3. Resolve the [engine.Host] — caller-supplied via WithEngineHost,
//     else [engine.NoopHost].
//  4. Build a [RunInfo] and notify all registered Observers via
//     OnRunStart.
//  5. Call eng.Execute. The engine mutates the board in place per
//     its contract.
//  6. If the engine returned an interrupt, fire OnInterrupt with the
//     destructured cause/detail.
//  7. Translate the engine outcome into Status and assemble [Result].
//  8. Run the Decider chain ([Decider.BeforeFinalize]) and merge the
//     decisions; this fixes [Result.Committed] and any
//     finalize_reason metadata.
//  9. Fire OnRunEnd before returning. Observers that persist data
//     (transcript appenders, artifact archivers) MUST inspect
//     Result.Committed and short-circuit when it is false.
//
// # Error contract
//
// Run returns (res, nil) for every business outcome — completion,
// interrupt, cancel, abort, failure. (nil, err) is reserved for
// infrastructure failures the caller cannot reasonably recover from:
// nil engine, empty Agent.ID, a BoardSeeder that returned an error,
// or a Decider that returned an error.
//
// Observers MUST NOT cause Run to return an error; they are
// advisory. Deciders may return errors that surface back to the
// caller — agent does not swap the error class so callers can
// classify with errdefs.
func Run(
	ctx context.Context,
	ag Agent,
	eng engine.Engine,
	req Request,
	opts ...RunOption,
) (*Result, error) {
	if eng == nil {
		return nil, errdefs.Validationf("agent: nil engine")
	}
	if ag.ID == "" {
		return nil, errdefs.Validationf("agent: Agent.ID is empty")
	}

	rc := applyOptions(ag, opts)

	runID := req.RunID
	if runID == "" {
		runID = mintRunID()
	}

	info := RunInfo{
		AgentID:   ag.ID,
		RunID:     runID,
		TaskID:    req.TaskID,
		ContextID: req.ContextID,
	}

	board, err := rc.seeder.SeedBoard(ctx, info, &req)
	if err != nil {
		return nil, fmt.Errorf("agent: seed board: %w", err)
	}
	if board == nil {
		return nil, errdefs.Validationf("agent: BoardSeeder returned nil board")
	}

	host := rc.host
	if host == nil {
		host = engine.NoopHost{}
	}

	attrs := mergeAttributes(rc.attributes, req, ag, runID)

	engRun := engine.Run{
		ID:         runID,
		Attributes: attrs,
		Deps:       rc.deps,
	}

	obs := composeObservers(rc.observers)

	if obs != nil {
		obs.OnRunStart(ctx, info, &req)
	}

	finalBoard, execErr := eng.Execute(ctx, engRun, host, board)
	if finalBoard == nil {
		// Defensive: engines must return a non-nil board even on
		// error per their contract. Fall back to the seeded one
		// rather than panic.
		finalBoard = board
	}

	res := &Result{
		TaskID:    req.TaskID,
		RunID:     runID,
		LastBoard: finalBoard,
		State:     map[string]any{"run_id": runID},
	}

	res.Messages = newAssistantMessages(finalBoard)

	switch {
	case execErr == nil:
		res.Status = StatusCompleted

	case errdefs.IsInterrupted(execErr):
		res.Status = StatusInterrupted
		res.Err = execErr
		// OnInterrupt requires the structured cause/detail. We only
		// fire it when errors.As surfaces a real engine.InterruptedError
		// — a foreign-shape error that merely satisfies the
		// errdefs.Interrupted marker is still classified as interrupted
		// (Status + IsInterrupted), but observers receive nothing
		// rather than a misleading zero-value Interrupt.
		var ie engine.InterruptedError
		if errors.As(execErr, &ie) {
			res.Cause = ie.Cause
			if obs != nil {
				obs.OnInterrupt(ctx, info, ie.Interrupt)
			}
		}

	case errors.Is(execErr, context.Canceled),
		errors.Is(execErr, context.DeadlineExceeded):
		res.Status = StatusCanceled
		res.Err = execErr

	case errdefs.IsAborted(execErr):
		res.Status = StatusAborted
		res.Err = execErr

	default:
		res.Status = StatusFailed
		res.Err = execErr
	}

	// Default disposition: only completed runs commit by default.
	res.Committed = res.Status == StatusCompleted

	if len(rc.deciders) > 0 {
		decision, derr := runDeciders(ctx, rc.deciders, info, &req, res)
		if derr != nil {
			// Observer's OnRunEnd still fires on the way out so
			// metric / log observers see the full lifecycle.
			if obs != nil {
				obs.OnRunEnd(ctx, info, res)
			}
			return res, derr
		}
		if decision.DiscardOutput {
			res.Committed = false
		}
		if decision.Reason != "" {
			res.State["finalize_reason"] = decision.Reason
		}
		// FinalizeDecision.Revise is reserved on the wire for a
		// later round; agent does not surface it to callers yet.
	}

	if obs != nil {
		obs.OnRunEnd(ctx, info, res)
	}

	return res, nil
}

// newAssistantMessages returns the assistant messages produced during
// the run by walking MainChannel from the end and collecting the
// trailing assistant block. Stops as soon as it hits a non-assistant
// message — earlier assistant messages are part of the seeded
// transcript, not output of this turn.
//
// This avoids the round-A "subtract the seeded prefix" book-keeping
// that depended on knowing how many messages the seeder injected.
// Trade-off: agents that interleave assistant + user turns inside one
// run will see only the trailing assistant block here. If that ever
// matters, callers can read finalBoard themselves via Result.LastBoard.
func newAssistantMessages(b *engine.Board) []model.Message {
	main := b.Channel(engine.MainChannel)
	end := len(main)
	start := end
	for start > 0 && main[start-1].Role == model.RoleAssistant {
		start--
	}
	if start == end {
		return nil
	}
	out := make([]model.Message, end-start)
	copy(out, main[start:end])
	return out
}

// mintRunID returns a fresh "run-<hex>" identifier. Falls back to a
// nanos-suffixed string if crypto/rand is unavailable (extremely rare
// — typically only sandboxes).
func mintRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b)
}

// mergeAttributes combines RunOption-supplied attributes with the
// well-known agent / task / context ids. Keys explicitly set by the
// caller win — agent never overwrites.
func mergeAttributes(extra map[string]string, req Request, ag Agent, runID string) map[string]string {
	out := make(map[string]string, len(extra)+4)
	out["agent_id"] = ag.ID
	out["run_id"] = runID
	if req.TaskID != "" {
		out["task_id"] = req.TaskID
	}
	if req.ContextID != "" {
		out["context_id"] = req.ContextID
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ---------- Options ----------

// RunOption configures one [Run] invocation. Options are
// stateless and may be reused across calls.
type RunOption func(*runConfig)

type runConfig struct {
	seeder     BoardSeeder
	deps       *engine.Dependencies
	host       engine.Host
	attributes map[string]string
	observers  []Observer
	deciders   []Decider
}

// applyOptions threads ag through so we can apply Agent-scoped
// observers / deciders before per-call ones, mirroring OpenClaw's
// "agent owns some hooks; the call adds more" pattern.
func applyOptions(ag Agent, opts []RunOption) runConfig {
	rc := runConfig{seeder: defaultSeeder{}}
	for _, h := range ag.Observers {
		if h != nil {
			rc.observers = append(rc.observers, h)
		}
	}
	for _, d := range ag.Deciders {
		if d != nil {
			rc.deciders = append(rc.deciders, d)
		}
	}
	for _, o := range opts {
		if o != nil {
			o(&rc)
		}
	}
	return rc
}

// WithBoardSeed installs a custom [BoardSeeder] for this run. Use it
// to inject conversation history, RAG-retrieved context, system
// prompts, or any other board state the engine needs at start.
//
// When omitted, agent uses [defaultSeeder] which appends req.Message
// to MainChannel and copies req.Inputs into board vars.
func WithBoardSeed(s BoardSeeder) RunOption {
	return func(rc *runConfig) {
		if s != nil {
			rc.seeder = s
		}
	}
}

// WithDependencies passes a dependency container to the engine via
// engine.Run.Deps. Engines look up named clients (LLM, retriever,
// tool registry, …) in there.
func WithDependencies(d *engine.Dependencies) RunOption {
	return func(rc *runConfig) { rc.deps = d }
}

// WithEngineHost installs the [engine.Host] passed to the engine.
//
// Host is the single extension point for every host-side capability
// the engine needs: event publishing (Publisher), interrupt injection
// (Interrupter), user prompting (UserPrompter), checkpoint
// persistence (Checkpointer), and token-usage reporting
// (UsageReporter). Composing your own host is how you wire any of
// those — agent does not provide narrow shortcuts because most
// non-trivial deployments share state across capabilities (a single
// metric client, a single OTel tracer, a single request-scoped
// logger) and a host implementation is the cleanest place to keep
// that state.
//
// Embed [engine.NoopHost] in your host struct and override only the
// methods you actually need:
//
//	type myHost struct {
//	    engine.NoopHost
//	    bus    event.Bus
//	    intrCh <-chan engine.Interrupt
//	}
//	func (h *myHost) Publish(ctx context.Context, e event.Envelope) error {
//	    return h.bus.Publish(ctx, e)
//	}
//	func (h *myHost) Interrupts() <-chan engine.Interrupt { return h.intrCh }
//
// When omitted, agent falls back to [engine.NoopHost], which silently
// drops envelopes, never fires interrupts, refuses AskUser, drops
// checkpoints, and discards usage. That default is appropriate for
// fire-and-forget batch runs and tests — anything else needs a real
// host.
func WithEngineHost(h engine.Host) RunOption {
	return func(rc *runConfig) { rc.host = h }
}

// WithAttributes adds extra attributes that flow into engine.Run.Attributes
// alongside the well-known agent_id / run_id / task_id / context_id keys.
// Caller-supplied keys win on conflict; agent does not overwrite.
func WithAttributes(extra map[string]string) RunOption {
	return func(rc *runConfig) {
		if rc.attributes == nil {
			rc.attributes = make(map[string]string, len(extra))
		}
		maps.Copy(rc.attributes, extra)
	}
}

// WithObserver registers a [Observer] for this run. Multiple
// observers can be registered; they fire in registration order, after
// any [Agent.Observers] declared on the agent value. Panics inside an
// observer are caught and dropped.
func WithObserver(o Observer) RunOption {
	return func(rc *runConfig) {
		if o != nil {
			rc.observers = append(rc.observers, o)
		}
	}
}

// WithDecider registers a [Decider] for this run. Multiple
// deciders can be registered; they fire in registration order, after
// any [Agent.Deciders] declared on the agent value. Their decisions
// are merged via OR over boolean fields; the first non-empty Reason
// wins.
func WithDecider(d Decider) RunOption {
	return func(rc *runConfig) {
		if d != nil {
			rc.deciders = append(rc.deciders, d)
		}
	}
}
