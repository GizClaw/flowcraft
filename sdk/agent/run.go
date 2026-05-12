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
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
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
	// Resume MUST execute under the original run id so the engine's
	// Resumer.CanResume sees ExecID == Run.ID. Honour the
	// checkpoint's ExecID over a freshly-minted id; explicit
	// req.RunID disagreements are caller errors and surface as
	// engine.Engine Validation when the engine compares them.
	if rc.resumeFrom != nil && rc.resumeFrom.ExecID != "" {
		runID = rc.resumeFrom.ExecID
	}

	info := RunInfo{
		AgentID:   ag.ID,
		RunID:     runID,
		TaskID:    req.TaskID,
		ContextID: req.ContextID,
	}

	host := rc.host
	if host == nil {
		host = engine.NoopHost{}
	}

	attrs := mergeAttributes(rc.attributes, req, ag, runID)
	// Promote agent.Agent.Tools into engine.Run.Deps under
	// depname.ToolAllowedNames so engines that honour the
	// allow-list (graph runner llmnode today; vessel inline engine
	// after Epic D) finally see the policy gate. Caller-supplied
	// rc.deps[ToolAllowedNames] wins so tests / power users can
	// override the agent-level claim per call.
	runDeps := promoteAgentTools(rc.deps, ag.Tools)
	obs := composeObservers(rc.observers)

	// Revise loop: each iteration is one engine.Execute attempt
	// followed by Decider chain. Loop exits when a Decider does
	// not ask for revise OR the attempt counter reaches the
	// configured WithMaxRevise budget. attempts is 1-indexed: 1
	// means "first engine call". maxAttempts >= 1 always (the
	// default 0 / 1 disable the loop entirely after the first
	// attempt — by zeroing rc.maxRevise we keep the math uniform).
	maxAttempts := rc.maxRevise
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var (
		res         *Result
		execDecided bool // set when a non-recoverable Decider error short-circuited
		decErr      error
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Re-seed board on every attempt so revise restarts see a
		// fresh state derived from the same Request. The first
		// attempt's seeder error is fatal (infrastructure); a seeder
		// error on a revise attempt is also surfaced as nil result +
		// error so callers do not silently see stale Messages.
		board, err := rc.seeder.SeedBoard(ctx, info, &req)
		if err != nil {
			return nil, fmt.Errorf("agent: seed board (attempt %d): %w", attempt, err)
		}
		if board == nil {
			return nil, errdefs.Validationf("agent: BoardSeeder returned nil board")
		}

		// engine.Run is rebuilt each attempt: ResumeFrom is honoured
		// for attempt 1 only (revise is not "resume", it is a fresh
		// retry), and Attributes carry the attempt index so engines
		// / hosts can correlate retries in their telemetry. RunID
		// stays constant across attempts so observers / dashboards
		// see one logical run with N attempts, not N separate runs.
		attemptAttrs := maps.Clone(attrs)
		if attemptAttrs == nil {
			attemptAttrs = map[string]string{}
		}
		attemptAttrs["agent.attempt"] = itoa(attempt)
		engRun := engine.Run{
			ID:          runID,
			ParentRunID: rc.parentRunID,
			Attributes:  attemptAttrs,
			Deps:        runDeps,
		}
		if attempt == 1 {
			engRun.ResumeFrom = rc.resumeFrom
		}

		if obs != nil {
			obs.OnRunStart(ctx, info, &req)
		}

		finalBoard, execErr := eng.Execute(ctx, engRun, host, board)
		if finalBoard == nil {
			finalBoard = board
		}

		res = &Result{
			TaskID:    req.TaskID,
			RunID:     runID,
			LastBoard: finalBoard,
			State:     map[string]any{"run_id": runID},
			Attempts:  attempt,
		}
		res.Messages = newAssistantMessages(finalBoard)

		switch {
		case execErr == nil:
			res.Status = StatusCompleted

		case errdefs.IsInterrupted(execErr):
			res.Status = StatusInterrupted
			res.Err = execErr
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

		res.Committed = res.Status == StatusCompleted

		// Non-completed outcomes short-circuit the revise loop. A
		// canceled / aborted / interrupted / failed engine is the
		// host's signal to stop; revising would either repeat the
		// failure mode (no model output to revise) or fight an
		// active cancellation. Deciders still run on the final
		// attempt so DiscardOutput / Reason are honoured.
		decision := FinalizeDecision{}
		if len(rc.deciders) > 0 {
			d, derr := runDeciders(ctx, rc.deciders, info, &req, res)
			if derr != nil {
				execDecided = true
				decErr = derr
				break
			}
			decision = d
			if decision.DiscardOutput {
				res.Committed = false
			}
			if decision.Reason != "" {
				res.State["finalize_reason"] = decision.Reason
			}
		}

		// Revise gate: only fires for completed attempts that have
		// budget remaining AND a Decider asked for revision. A
		// non-completed status NEVER triggers revise (see comment
		// above) so a flapping engine cannot consume the entire
		// budget against transient failures.
		if !decision.Revise || res.Status != StatusCompleted || attempt >= maxAttempts {
			break
		}
		if obs != nil {
			obs.OnRunRevise(ctx, info, res, attempt+1)
		}
	}

	if execDecided {
		if obs != nil {
			obs.OnRunEnd(ctx, info, res)
		}
		return res, decErr
	}

	if obs != nil {
		obs.OnRunEnd(ctx, info, res)
	}

	return res, nil
}

// itoa is a zero-alloc small-int formatter used for the attempt
// attribute. strconv.Itoa would work but pulls in strconv just for
// this single callsite; the manual base-10 conversion is small
// enough to keep inline.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
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

// promoteAgentTools is the agent-level policy gate hand-off. When
// agent.Agent.Tools is set and the caller's Dependencies container
// does NOT already define [depname.ToolAllowedNames], the helper
// returns a CLONE of the caller's container with the agent's tool
// list set under that key. Engines that honour the allow-list
// (graph runner llmnode; vessel inline engine after Epic D) then
// see it via engine.GetDep at run time.
//
// Cloning preserves Run's "callers' container is immutable from
// our perspective" rule — a caller that reuses one Dependencies
// across many runs (different agents) won't see this run's tool
// list bleed into the next.
//
// Caller-supplied wins: if the caller already set
// depname.ToolAllowedNames on the container (e.g. tests, or a
// power user overriding the agent claim), the helper returns the
// container unchanged and the agent's Tools field is silently
// shadowed for this call. This matches the "caller-supplied wins"
// rule mergeAttributes uses for the attribute bag.
//
// When agent.Tools is nil/empty the helper is a no-op; agents that
// do not opt into the policy gate keep getting the caller's deps
// verbatim (back-compat for code that hasn't yet started populating
// agent.Tools).
func promoteAgentTools(callerDeps *engine.Dependencies, agentTools []string) *engine.Dependencies {
	if len(agentTools) == 0 {
		return callerDeps
	}
	if callerDeps != nil && callerDeps.Has(depname.ToolAllowedNames) {
		return callerDeps
	}
	cloned := callerDeps.Clone()
	if cloned == nil {
		cloned = engine.NewDependencies()
	}
	// Defensive copy of agentTools — agent owns ag.Tools and may
	// mutate it after Run returns; the engine should see a stable
	// snapshot for the duration of this call.
	tools := append([]string(nil), agentTools...)
	cloned.Set(depname.ToolAllowedNames, tools)
	return cloned
}

// mergeAttributes combines RunOption-supplied attributes with the
// well-known agent / task / context ids. Keys explicitly set by the
// caller win — agent never overwrites.
//
// Key names live in runinfo_attrs.go and stay private to this
// package; downstream readers go through [RunInfoFromAttributes] so
// the wire format can be migrated without sweeping the codebase.
func mergeAttributes(extra map[string]string, req Request, ag Agent, runID string) map[string]string {
	out := make(map[string]string, len(extra)+4)
	out[attrAgentID] = ag.ID
	out[attrRunID] = runID
	if req.TaskID != "" {
		out[attrTaskID] = req.TaskID
	}
	if req.ContextID != "" {
		out[attrContextID] = req.ContextID
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
	seeder      BoardSeeder
	deps        *engine.Dependencies
	host        engine.Host
	attributes  map[string]string
	observers   []Observer
	deciders    []Decider
	resumeFrom  *engine.Checkpoint
	runID       string
	parentRunID string
	maxRevise   int
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
//
// This is also the canonical replacement for the deprecated
// [Request.Extensions] (contract-audit #8): engines that need
// caller-supplied metadata read engine.Run.Attributes via the same
// codepath as the well-known keys, with no map[string]any →
// map[string]string serialisation guesswork. Hosts that previously
// wrote into req.Extensions should serialise the values at the
// call site and pass the resulting map[string]string here.
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

// WithMaxRevise sets the upper bound on engine.Execute invocations
// per Run call when a Decider returns FinalizeDecision{Revise: true}.
//
//   - n <= 1 (default 0) disables the revise loop entirely. A Decider
//     asking to revise records its Reason but Run still returns after
//     the first attempt — the safe default avoids surprise infinite
//     loops on misconfigured Deciders.
//
//   - n >= 2 caps total attempts at n. The loop exits as soon as
//     either no Decider asks for revise OR the attempt counter
//     reaches n. The final Result.Attempts is the actual number of
//     engine.Execute calls made.
//
// Revise restarts re-seed the board from the original Request via
// the configured BoardSeeder, so the engine sees fresh inputs.
// engine.Run.ResumeFrom is dropped after the first attempt — Revise
// means "retry from scratch", not "replay a checkpoint".
//
// Negative values are treated as 0 (disabled). Callers that want the
// engine to drive its own retry policy (rate-limit backoff, transient
// LLM errors, …) MUST keep WithMaxRevise at the default; the revise
// loop is the agent-policy layer, not the engine-transport one.
func WithMaxRevise(n int) RunOption {
	return func(rc *runConfig) {
		if n < 0 {
			n = 0
		}
		rc.maxRevise = n
	}
}

// WithParentRunID stamps every engine.Run this call dispatches with
// the supplied parent run id (engine.Run.ParentRunID). Use it when
// one agent.Run is spawned by another (multi-agent call chain,
// handoff, sub-agent dispatch) so dashboards / pod controllers can
// reconstruct the call tree and apply loop-detection / depth budgets
// against a stable correlation key.
//
// The empty string is a no-op; passing the parent's runID
// (typically obtained from agent.RunInfo.RunID inside an Observer
// / Decider on the parent run) is the canonical use. agent.Run does
// NOT auto-derive ParentRunID from any ambient context — explicit
// is the only contract that survives ctx propagation rewrites and
// cross-process dispatch (vessel, A2A bridge).
//
// Engines / hosts that don't read ParentRunID are unaffected. The
// field is also surfaced under telemetry.AttrParentRunID by
// observers that emit run-summary spans (sdk/telemetry/run_summary).
func WithParentRunID(id string) RunOption {
	return func(rc *runConfig) { rc.parentRunID = id }
}

// WithResumeFrom replays an interrupted run from a previously
// captured engine.Checkpoint. The agent threads cp into
// engine.Run.ResumeFrom and overrides the run id to cp.ExecID so
// the underlying engine's Resumer.CanResume sees ExecID == Run.ID
// (cross-run checkpoints are programmer errors and surface as
// errdefs.Validation from the engine).
//
// Typical use: a host loaded a checkpoint via its CheckpointStore,
// possibly after a process restart, and wants the agent to keep
// going from that point rather than start fresh. The host still
// passes the ORIGINAL agent.Request (same task id, same inputs);
// the engine restores board state from the checkpoint so the
// re-seeded inputs are effectively overwritten by the resumed
// state. Engines without engine.Resumer surface NotAvailable
// (per the engine.Engine contract); resume against an unsupported
// engine is a configuration error, not silent fall-through.
//
// nil cp is a no-op (= fresh start). Multiple WithResumeFrom calls
// last-write-wins; agent does not attempt to merge checkpoints.
func WithResumeFrom(cp *engine.Checkpoint) RunOption {
	return func(rc *runConfig) { rc.resumeFrom = cp }
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
