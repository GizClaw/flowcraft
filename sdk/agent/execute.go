package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

// Execute executes one turn of ag against eng with the given req.
//
// Execute is intentionally minimalist: it owns identifier minting,
// board assembly, hook dispatch, and result classification — and
// nothing else. Anything that looks like "policy" (load conversation
// history, run RAG retrieval, write transcripts after a turn, emit
// metrics, route engine envelopes to a bus, accumulate token usage,
// …) lives outside Execute on:
//
//   - [Hook] / [AfterExecute] for lifecycle hooks;
//   - [BeforeExecute] for engine-input shaping;
//   - the caller-supplied [Host] (see [WithHost]) for
//     every host-side capability the engine needs (event publishing,
//     interrupt injection, user prompting, checkpoint persistence,
//     usage reporting). When omitted, agent falls back to
//     [NoopHost] — which is fine for trivial / test runs but
//     gives up every observability and HITL capability.
//
// # Wiring sequence
//
//  1. Mint a RunID (req.RunID wins, else autogenerate).
//  2. Build an [Board] using either a caller-supplied
//     BeforeExecute ([WithBeforeExecute]) or the default seeder, which
//     simply appends req.Message to MainChannel and copies
//     req.Inputs to board vars.
//  3. Resolve the [Host] — caller-supplied via WithHost,
//     else [NoopHost].
//  4. Build a [Identity] and notify all registered Hooks via
//     OnRunStart.
//  5. Call eng.Execute. The engine mutates the board in place per
//     its contract.
//  6. If the engine returned an interrupt, fire OnInterrupt with the
//     destructured cause/detail.
//  7. Translate the engine outcome into Status and assemble [Result].
//  8. Run the AfterExecute chain ([AfterExecute.After]) and merge the
//     decisions; this fixes [Result.Committed] and any
//     finalize_reason metadata.
//  9. Fire OnRunEnd before returning. Hooks that persist data
//     (transcript appenders, artifact archivers) MUST inspect
//     Result.Committed and short-circuit when it is false.
//
// # Error contract
//
// Execute returns (res, nil) for every business outcome — completion,
// interrupt, cancel, abort, failure. (nil, err) is reserved for
// infrastructure failures the caller cannot reasonably recover from:
// nil engine, empty Agent.ID, a BeforeExecute that returned an error,
// or a AfterExecute that returned an error.
//
// Hooks MUST NOT cause Execute to return an error; they are
// advisory. After may return errors that surface back to the
// caller — agent does not swap the error class so callers can
// classify with errdefs.
func Execute(
	ctx context.Context,
	ag Agent,
	eng Engine,
	req Request,
	opts ...ExecuteOption,
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
	// Resumer.CanResume sees ExecID == Run.RunID. Honour the
	// checkpoint's ExecID over a freshly-minted id; explicit
	// req.RunID disagreements are caller errors and surface as
	// Engine Validation when the engine compares them.
	if rc.resumeFrom != nil && rc.resumeFrom.ExecID != "" {
		runID = rc.resumeFrom.ExecID
	}

	id := Identity{
		AgentID:        ag.ID,
		RunID:          runID,
		ParentRunID:    rc.parentRunID,
		TaskID:         req.TaskID,
		ConversationID: req.ContextID,
	}

	host := rc.host
	if host == nil {
		host = NoopHost{}
	}
	// Tool allow-list: policy, not wiring. Caller-supplied
	// ([WithToolAllowList]) wins over the agent-level claim so
	// tests / power users can override per call. Defensive copy —
	// the engine must see a stable snapshot for the duration of
	// the run even if the caller mutates their slice afterwards.
	toolAllowList := ag.Tools
	if rc.toolAllowList != nil {
		toolAllowList = rc.toolAllowList
	}
	toolAllowList = append([]string(nil), toolAllowList...)
	obs := composeHooks(rc.hooks)

	// Revise loop: each iteration is one Execute attempt
	// followed by AfterExecute chain. Loop exits when a AfterExecute does
	// not ask for revise OR the attempt counter reaches the
	// configured WithMaxRevise budget. attempts is 1-indexed: 1
	// means "first engine call". maxAttempts >= 1 always (the
	// default 0 / 1 disable the loop entirely after the first
	// attempt — by zeroing rc.maxRevise we keep the math uniform).
	maxAttempts := max(rc.maxRevise, 1)

	var (
		res         *Result
		execDecided bool // set when a non-recoverable AfterExecute error short-circuited
		decErr      error
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Re-seed board on every attempt so revise restarts see a
		// fresh state derived from the same Request. The first
		// attempt's seeder error is fatal (infrastructure); a seeder
		// error on a revise attempt is also surfaced as nil result +
		// error so callers do not silently see stale Messages.
		board, err := rc.before.Before(ctx, id, &req)
		if err != nil {
			return nil, fmt.Errorf("agent: seed board (attempt %d): %w", attempt, err)
		}
		if board == nil {
			return nil, errdefs.Validationf("agent: BeforeExecute returned nil board")
		}

		// Run is rebuilt each attempt: ResumeFrom is honoured
		// for attempt 1 only (revise is not "resume", it is a fresh
		// retry), and Attributes carry the attempt index so engines
		// / hosts can correlate retries in their telemetry. RunID
		// stays constant across attempts so observers / dashboards
		// see one logical run with N attempts, not N separate runs.
		attemptAttrs := maps.Clone(rc.attributes)
		if attemptAttrs == nil {
			attemptAttrs = map[string]string{}
		}
		attemptAttrs["agent.attempt"] = itoa(attempt)
		engRun := Run{
			Identity:      id,
			Attributes:    attemptAttrs,
			ToolAllowList: toolAllowList,
		}
		if attempt == 1 {
			engRun.ResumeFrom = rc.resumeFrom
		}

		if obs != nil {
			obs.OnRunStart(ctx, id, &req)
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
		res.Artifacts = collectArtifacts(finalBoard, rc.artifactChannels)

		switch {
		case execErr == nil:
			res.Status = StatusCompleted

		case errdefs.IsInterrupted(execErr):
			res.Status = StatusInterrupted
			res.Err = execErr
			var ie InterruptedError
			if errors.As(execErr, &ie) {
				res.Cause = ie.Cause
				if obs != nil {
					obs.OnInterrupt(ctx, id, ie.Interrupt)
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
		// active cancellation. After still run on the final
		// attempt so DiscardOutput / Reason are honoured.
		decision := Decision{}
		if len(rc.afters) > 0 {
			d, derr := runAfterExecute(ctx, rc.afters, id, &req, res)
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
		// budget remaining AND a AfterExecute asked for revision. A
		// non-completed status NEVER triggers revise (see comment
		// above) so a flapping engine cannot consume the entire
		// budget against transient failures.
		if !decision.Revise || res.Status != StatusCompleted || attempt >= maxAttempts {
			break
		}
		if obs != nil {
			obs.OnRunRevise(ctx, id, res, attempt+1)
		}
	}

	if execDecided {
		if obs != nil {
			obs.OnRunEnd(ctx, id, res)
		}
		return res, decErr
	}

	if obs != nil {
		obs.OnRunEnd(ctx, id, res)
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
func newAssistantMessages(b *Board) []inference.Message {
	main := b.Channel(MainChannel)
	end := len(main)
	start := end
	for start > 0 && main[start-1].Role == inference.RoleAssistant {
		start--
	}
	if start == end {
		return nil
	}
	out := make([]inference.Message, end-start)
	copy(out, main[start:end])
	return out
}

// collectArtifacts harvests the named board channels into Result.Artifacts
// per the [WithArtifactChannels] contract. One Artifact per channel that
// holds at least one Part after the run; Parts are flat-concatenated in
// board-write order. Channels are processed in the order callers
// registered them and de-duplicated so accumulating per-agent + per-call
// lists never produces duplicate Artifact entries.
//
// Channels that hold messages but no Parts (e.g. role-only system
// markers) produce no Artifact entry — empty Parts would be confusing
// to consumers that expect "Artifact present means there is something
// to render".
//
// nil channels list returns nil so the unaltered nil-Artifacts default
// for callers that don't opt in is preserved.
func collectArtifacts(b *Board, channels []string) []Artifact {
	if len(channels) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(channels))
	var out []Artifact
	for _, name := range channels {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		msgs := b.Channel(name)
		if len(msgs) == 0 {
			continue
		}
		var parts []inference.Part
		for _, m := range msgs {
			parts = append(parts, m.Content.Parts...)
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, Artifact{Name: name, Parts: parts})
	}
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

// ---------- Options ----------

// ExecuteOption configures one [Run] invocation. Options are
// stateless and may be reused across calls.
type ExecuteOption func(*execConfig)

type execConfig struct {
	before           BeforeExecute
	toolAllowList    []string
	host             Host
	attributes       map[string]string
	hooks            []Hook
	afters           []AfterExecute
	resumeFrom       *Checkpoint
	runID            string
	parentRunID      string
	maxRevise        int
	artifactChannels []string
}

// applyOptions threads ag through so we can apply Agent-scoped
// observers / deciders before per-call ones, mirroring OpenClaw's
// "agent owns some hooks; the call adds more" pattern.
func applyOptions(ag Agent, opts []ExecuteOption) execConfig {
	rc := execConfig{before: defaultBefore{}}
	for _, h := range ag.Hooks {
		if h != nil {
			rc.hooks = append(rc.hooks, h)
		}
	}
	for _, d := range ag.After {
		if d != nil {
			rc.afters = append(rc.afters, d)
		}
	}
	for _, o := range opts {
		if o != nil {
			o(&rc)
		}
	}
	return rc
}

// WithBeforeExecute installs a custom [BeforeExecute] for this run. Use it
// to inject conversation history, RAG-retrieved context, system
// prompts, or any other board state the engine needs at start.
//
// When omitted, agent uses [defaultBefore] which appends req.Message
// to MainChannel and copies req.Inputs into board vars.
func WithBeforeExecute(s BeforeExecute) ExecuteOption {
	return func(rc *execConfig) {
		if s != nil {
			rc.before = s
		}
	}
}

// WithToolAllowList overrides [Agent.Tools] for this call — the
// caller-supplied list wins wholesale (no merge), mirroring the
// "caller-supplied wins" rule [WithAttributes] documents. Pass it
// from tests or from power-user flows that narrow an agent's tool
// gate for one specific turn.
func WithToolAllowList(names []string) ExecuteOption {
	return func(rc *execConfig) { rc.toolAllowList = names }
}

// WithHost installs the [Host] passed to the
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
// Embed [NoopHost] in your host struct and override only the
// methods you actually need:
//
//	type myHost struct {
//	    NoopHost
//	    bus    event.Bus
//	    intrCh <-chan Interrupt
//	}
//	func (h *myHost) Publish(ctx context.Context, e event.Envelope) error {
//	    return h.bus.Publish(ctx, e)
//	}
//	func (h *myHost) Interrupts() <-chan Interrupt { return h.intrCh }
//
// When omitted, agent falls back to [NoopHost], which silently
// drops envelopes, never fires interrupts, refuses AskUser, drops
// checkpoints, and discards usage. That default is appropriate for
// fire-and-forget batch runs and tests — anything else needs a real
// host.
func WithHost(h Host) ExecuteOption {
	return func(rc *execConfig) { rc.host = h }
}

// WithAttributes adds extra attributes that flow into Run.Attributes
// alongside the well-known agent_id / run_id / task_id / context_id keys.
// Caller-supplied keys win on conflict; agent does not overwrite.
//
// Engines that need caller-supplied metadata read
// Run.Attributes via the same codepath as the well-known keys.
// Callers serialise any non-string values at the call site and pass
// the resulting map[string]string here.
func WithAttributes(extra map[string]string) ExecuteOption {
	return func(rc *execConfig) {
		if rc.attributes == nil {
			rc.attributes = make(map[string]string, len(extra))
		}
		maps.Copy(rc.attributes, extra)
	}
}

// WithHook registers a [Hook] for this run. Multiple
// observers can be registered; they fire in registration order, after
// any [Agent.Hooks] declared on the agent value. Panics inside an
// observer are caught and dropped.
func WithHook(o Hook) ExecuteOption {
	return func(rc *execConfig) {
		if o != nil {
			rc.hooks = append(rc.hooks, o)
		}
	}
}

// WithMaxRevise sets the upper bound on Execute invocations
// per Run call when a AfterExecute returns Decision{Revise: true}.
//
//   - n <= 1 (default 0) disables the revise loop entirely. A AfterExecute
//     asking to revise records its Reason but Run still returns after
//     the first attempt — the safe default avoids surprise infinite
//     loops on misconfigured After.
//
//   - n >= 2 caps total attempts at n. The loop exits as soon as
//     either no AfterExecute asks for revise OR the attempt counter
//     reaches n. The final Result.Attempts is the actual number of
//     Execute calls made.
//
// Revise restarts re-seed the board from the original Request via
// the configured BeforeExecute, so the engine sees fresh inputs.
// Run.ResumeFrom is dropped after the first attempt — Revise
// means "retry from scratch", not "replay a checkpoint".
//
// Negative values are treated as 0 (disabled). Callers that want the
// engine to drive its own retry policy (rate-limit backoff, transient
// LLM errors, …) MUST keep WithMaxRevise at the default; the revise
// loop is the agent-policy layer, not the engine-transport one.
func WithMaxRevise(n int) ExecuteOption {
	return func(rc *execConfig) {
		if n < 0 {
			n = 0
		}
		rc.maxRevise = n
	}
}

// WithArtifactChannels names the [Board] channels [Run] should
// harvest into [Result.Artifacts] on the way out. One Artifact per
// listed channel: Artifact.Name = channel name; Artifact.Parts =
// flat concatenation of every Part across every Message in the
// channel (in board-write order). Channels that hold no messages
// after the run produce no Artifact entry — empty channels do not
// pollute the result with empty bundles.
//
// This is the writer side of the Result.Artifacts contract that
// contract-audit #6 flagged: the godoc had been promising
// "engines store them in a board channel; agent collects channel
// contents into Artifacts on the way out" since v0.1, but no
// agent.Run code path actually performed the collection so the
// field was permanently nil for every caller.
//
// MUST NOT include [MainChannel] — Run already promotes
// that channel into [Result.Messages] and a duplicate harvest
// would surface the same payload twice with confusing semantics
// (Messages keeps role + tool metadata; Artifacts is the
// modality-bundle view). Run silently skips MainChannel if the
// caller mistakenly passes it.
//
// Example: an engine that writes a "summary" markdown blob and
// an "audio" TTS clip to dedicated channels:
//
//	res, _ := agent.Execute(ctx, ag, eng, req,
//	    agent.WithArtifactChannels("summary", "audio"))
//	for _, a := range res.Artifacts {
//	    switch a.Name { ... }
//	}
//
// Multiple WithArtifactChannels calls accumulate (deduped at
// collection time) so per-agent and per-call lists compose. nil
// / empty input is a no-op.
func WithArtifactChannels(channels ...string) ExecuteOption {
	return func(rc *execConfig) {
		for _, c := range channels {
			if c == "" || c == MainChannel {
				continue
			}
			rc.artifactChannels = append(rc.artifactChannels, c)
		}
	}
}

// WithParentRunID stamps every Run this call dispatches with
// the supplied parent run id (Run.ParentRunID). Use it when
// one agent.Run is spawned by another (multi-agent call chain,
// handoff, sub-agent dispatch) so dashboards / pod controllers can
// reconstruct the call tree and apply loop-detection / depth budgets
// against a stable correlation key.
//
// The empty string is a no-op; passing the parent's runID
// (typically obtained from agent.Identity.RunID inside an Hook
// / AfterExecute on the parent run) is the canonical use. agent.Execute does
// NOT auto-derive ParentRunID from any ambient context — explicit
// is the only contract that survives ctx propagation rewrites and
// cross-process dispatch (application runtimes, A2A bridges).
//
// Engines / hosts that don't read ParentRunID are unaffected. The
// field is also surfaced under telemetry.AttrParentRunID by
// observers that emit run-summary spans (sdk/telemetry/run_summary).
func WithParentRunID(id string) ExecuteOption {
	return func(rc *execConfig) { rc.parentRunID = id }
}

// WithResumeFrom replays an interrupted run from a previously
// captured Checkpoint. The agent threads cp into
// Run.ResumeFrom and overrides the run id to cp.ExecID so
// the underlying engine's Resumer.CanResume sees ExecID == Run.RunID
// (cross-run checkpoints are programmer errors and surface as
// errdefs.Validation from the engine).
//
// Typical use: a host loaded a checkpoint via its CheckpointStore,
// possibly after a process restart, and wants the agent to keep
// going from that point rather than start fresh. The host still
// passes the ORIGINAL agent.Request (same task id, same inputs);
// the engine restores board state from the checkpoint so the
// re-seeded inputs are effectively overwritten by the resumed
// state. Engines without Resumer surface NotAvailable
// (per the Engine contract); resume against an unsupported
// engine is a configuration error, not silent fall-through.
//
// nil cp is a no-op (= fresh start). Multiple WithResumeFrom calls
// last-write-wins; agent does not attempt to merge checkpoints.
func WithResumeFrom(cp *Checkpoint) ExecuteOption {
	return func(rc *execConfig) { rc.resumeFrom = cp }
}

// WithAfterExecute registers a [AfterExecute] for this run. Multiple
// deciders can be registered; they fire in registration order, after
// any [Agent.After] declared on the agent value. Their decisions
// are merged via OR over boolean fields; the first non-empty Reason
// wins.
func WithAfterExecute(d AfterExecute) ExecuteOption {
	return func(rc *execConfig) {
		if d != nil {
			rc.afters = append(rc.afters, d)
		}
	}
}

// ---------- Run descriptor ----------

// Identity is the strongly-typed identity bundle for one execution.
// It is a typed value (rather than loose attributes) so the
// downstream executor sees the same identity the harness minted.
type Identity struct {
	// AgentID identifies the [Agent] producing this run. Engines
	// stamp it onto envelope headers (event.HeaderAgentID) and use
	// it as the agent segment of step subjects.
	AgentID string `json:"agent_id,omitempty"`

	// RunID is the unique identifier of this execution. The host
	// generates it and keeps it stable across resume / checkpoint
	// cycles; revise attempts reuse the same RunID so observers see
	// one logical run with N attempts.
	RunID string `json:"run_id,omitempty"`

	// ParentRunID identifies the parent run when this run was
	// dispatched by another agent (multi-agent call chains). Empty
	// for top-level runs. Typed (rather than an attribute) so
	// loop / depth detection at the pod layer can rely on the
	// contract.
	ParentRunID string `json:"parent_run_id,omitempty"`

	// TaskID scopes this run to a long-running A2A task. Empty when
	// the caller did not supply one.
	TaskID string `json:"task_id,omitempty"`

	// ConversationID names the thread-of-interaction scope (A2A
	// contextId semantics). Empty when the turn is unscoped.
	ConversationID string `json:"conversation_id,omitempty"`
}

// Run is the per-execution input bundle an engine receives alongside
// the host. It is a plain data struct — no methods, no builder, no
// hidden state — assembled once by the host and passed to
// [Engine.Execute] read-only.
//
// All fields are conceptually immutable for the duration of the run.
// Engines may read freely; they MUST NOT mutate the maps in place
// nor mutate the referenced ResumeFrom checkpoint.
type Run struct {
	// Identity is the run's identity envelope. All identity
	// dimensions are typed fields here — read run.AgentID /
	// run.RunID / run.TaskID directly. (They resolve through the
	// embedded struct, e.g. run.RunID == run.Identity.RunID.)
	Identity

	// Attributes carries arbitrary host-supplied metadata that should
	// flow into telemetry spans and event headers (tenant id, engine
	// kind, feature flags, …). Identity dimensions do NOT live here
	// — they are typed fields on [Identity].
	//
	// Convention: keys SHOULD use the constants in sdk/telemetry
	// (`telemetry.AttrTenantID`, `telemetry.AttrEngineKind`, …) so
	// cross-package consumers (dashboards, log queries) can filter
	// without per-package translation rules.
	Attributes map[string]string

	// ToolAllowList is the policy gate naming the tool ids this run
	// is permitted to call. Engines that invoke tools honour it;
	// empty means "no agent-level restriction" (the engine's own
	// default applies). It is policy, not wiring — tool
	// *implementations* are bound at engine assembly time (see
	// [Factory] / [Config]), never per run.
	ToolAllowList []string

	// ResumeFrom, when non-nil, instructs the engine to continue
	// execution from the provided checkpoint instead of starting a
	// fresh run. The engine is the sole interpreter of
	// [Checkpoint.Step] and [Checkpoint.Payload]; the host treats
	// them as opaque.
	//
	// Contract:
	//
	//   - When ResumeFrom is non-nil the engine SHOULD prefer
	//     ResumeFrom.Board over the board parameter passed to
	//     [Engine.Execute]; passing both is allowed but the
	//     checkpoint's board takes precedence as it represents the
	//     state at the boundary the run paused on.
	//
	//   - When ResumeFrom.ExecID differs from [Run.RunID] the engine
	//     MUST return an errdefs.Validation-classified error: forking
	//     a run requires a fresh execution id, not a resume.
	//
	//   - Engines that do not support resume MUST return an
	//     errdefs.NotAvailable-classified error when they observe a
	//     non-nil ResumeFrom rather than silently restarting from
	//     scratch.
	//
	// Hosts that drive resumption typically [CheckpointStore.Load]
	// the most recent checkpoint, set ResumeFrom, and call
	// [Engine.Execute] again with the same Run.RunID.
	ResumeFrom *Checkpoint
}

// Attribute returns the value for the given attribute key, or "" if
// absent. A small convenience over `r.Attributes[key]` that handles
// a nil Attributes map.
func (r Run) Attribute(key string) string {
	if r.Attributes == nil {
		return ""
	}
	return r.Attributes[key]
}

// RunInfo is the read-only, node-facing projection of [Run].
//
// Engines hand it to step/node implementations instead of the full
// Run: everything a step legitimately needs (identity, attributes,
// tool policy) is here, while engine-internal machinery such as
// [Run.ResumeFrom] stays invisible. The projection is owned by this
// package — when Run grows a new field, the author decides here
// whether steps should see it, keeping every engine's step API stable.
type RunInfo struct {
	Identity

	// Attributes mirrors [Run.Attributes] (host-supplied metadata:
	// tenant id, engine kind, feature flags, …).
	Attributes map[string]string

	// ToolAllowList mirrors [Run.ToolAllowList]: the tool ids this run
	// is permitted to call. Empty means "no agent-level restriction".
	ToolAllowList []string
}

// Info returns the node-facing projection of r.
func (r Run) Info() RunInfo {
	return RunInfo{
		Identity:      r.Identity,
		Attributes:    r.Attributes,
		ToolAllowList: r.ToolAllowList,
	}
}

// Attribute returns the value for the given attribute key, or "" if
// absent, tolerating a nil Attributes map.
func (ri RunInfo) Attribute(key string) string {
	if ri.Attributes == nil {
		return ""
	}
	return ri.Attributes[key]
}

type runInfoCtxKey struct{}

// WithRunInfo returns a derived context carrying info. Run identity
// is AMBIENT: deep components (script bindings, stream adapters,
// helpers far below the engine's call sites) read it from the
// context instead of receiving it through every intermediate
// signature. Engines inject it once at the run boundary.
func WithRunInfo(ctx context.Context, info RunInfo) context.Context {
	return context.WithValue(ctx, runInfoCtxKey{}, info)
}

// RunInfoFromContext returns the RunInfo attached to ctx, plus an
// "ok" flag. ok=false means the caller is outside an engine run (or
// the engine did not populate the context) — consumers should treat
// the zero RunInfo as "identity unknown" rather than an error.
func RunInfoFromContext(ctx context.Context) (RunInfo, bool) {
	if ctx == nil {
		return RunInfo{}, false
	}
	info, ok := ctx.Value(runInfoCtxKey{}).(RunInfo)
	return info, ok
}

// ---------- Resume ----------

// Resumer is the optional capability an [Engine] may advertise to
// signal it can drive [Run.ResumeFrom] and to short-circuit obvious
// mismatches *before* the host spins up an execution.
//
// The interface is intentionally minimal — a single CanResume probe
// — so engines opt in cheaply and hosts can build resume tooling
// (admin UIs, CLI commands, supervised retry loops) without a full
// dry-run.
//
// Engines that do NOT implement Resumer remain fully spec-compliant:
// the [Run.ResumeFrom] contract still applies and they MUST return
// an [errdefs.NotAvailable]-classified error if asked to resume. The
// helpers below ([IsResumable], [LoadAndResume]) treat the absence
// of Resumer as "engine handles resume opaquely; trust Execute to
// surface the right error".
type Resumer interface {
	// CanResume returns nil if the given checkpoint is resumable by
	// this engine, or a classified error explaining why not:
	//
	//   - errdefs.Validation: checkpoint shape is wrong (engine kind
	//     mismatch, missing required Payload fields, ExecID
	//     conflict).
	//   - errdefs.NotAvailable: engine recognises the checkpoint but
	//     cannot resume it (incompatible engine version, removed
	//     node type, etc.).
	//
	// Implementations MUST be cheap (no I/O, no LLM calls); the
	// probe runs synchronously on the host's resume path.
	CanResume(cp Checkpoint) error
}

// IsResumable reports whether eng implements [Resumer]. Use the
// two-value variant when you want the typed handle:
//
//	if r, ok := engine.AsResumer(eng); ok { _ = r.CanResume(cp) }
func IsResumable(eng Engine) bool {
	_, ok := AsResumer(eng)
	return ok
}

// AsResumer is the typed counterpart of [IsResumable]. It walks
// wrappers that expose Unwrap() Engine (errors.As-style) so a
// Resumer hidden under decorator wrappers still surfaces correctly.
func AsResumer(eng Engine) (Resumer, bool) {
	for eng != nil {
		if r, ok := eng.(Resumer); ok {
			return r, true
		}
		u, ok := eng.(interface{ Unwrap() Engine })
		if !ok {
			return nil, false
		}
		eng = u.Unwrap()
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// ResumeContext — per-run resume metadata threaded through context.Context
// ---------------------------------------------------------------------------

// ResumeContext is the auxiliary metadata about the *current* resume
// attempt. Engines, observers and middleware read it from
// context.Context to differentiate fresh starts from resumes, count
// attempts in retry loops, and surface "this is replay" indicators
// in trace UIs.
//
// ResumeContext is distinct from [Run.ResumeFrom]: the checkpoint
// describes WHERE the engine should pick up; ResumeContext describes
// WHY the resume is happening (who triggered it, which attempt this
// is, how long the original run has been alive). Keep them separate
// so hosts can drive replay + retry semantics without mutating the
// checkpoint payload.
type ResumeContext struct {
	// Attempt is 1-based: the very first attempt (fresh run, no
	// resume) is 1; the first re-execution after a checkpoint is
	// 2; and so on. Hosts that limit retry budget read this field.
	Attempt int

	// StartedAt is the wall-clock time the original [Run] began.
	// Stays constant across resumes so dashboards can compute
	// total wall time (e.g. SLO budget burn).
	StartedAt time.Time

	// Signal identifies the trigger that produced THIS resume.
	// Convention (extensible — engines/middleware MUST treat
	// unknown values as opaque):
	//
	//   - "manual"           — operator clicked Resume / CLI
	//   - "interrupt-recovery" — host re-executed after a host.Interrupts() Stop
	//   - "schedule"         — cron / queue replayer
	//   - "crash"            — supervisor restarted after process exit
	//   - "retry"            — automated retry on classified failure
	Signal string

	// CheckpointAt is when the checkpoint that fuels this resume
	// was originally produced (the engine's view of "paused at").
	// Empty time.Time on fresh starts.
	CheckpointAt time.Time
}

type resumeCtxKey struct{}

// WithResumeContext returns a derived context carrying rc. Engines
// that publish telemetry attribs (attempt count, resume signal)
// pull from here instead of plumbing extra parameters.
func WithResumeContext(ctx context.Context, rc ResumeContext) context.Context {
	return context.WithValue(ctx, resumeCtxKey{}, rc)
}

// ResumeContextFromContext returns the ResumeContext attached to
// ctx, plus an "ok" flag. The ok=false branch means the engine is
// running a fresh start (or the host did not bother to populate
// the context — engines should treat both cases identically).
func ResumeContextFromContext(ctx context.Context) (ResumeContext, bool) {
	if ctx == nil {
		return ResumeContext{}, false
	}
	rc, ok := ctx.Value(resumeCtxKey{}).(ResumeContext)
	return rc, ok
}

// ---------------------------------------------------------------------------
// LoadAndResume — high-level helper that wires CheckpointStore + Engine
// ---------------------------------------------------------------------------

// LoadAndResumeOption tunes [LoadAndResume] behaviour.
type LoadAndResumeOption func(*loadAndResumeOpts)

type loadAndResumeOpts struct {
	signal       string
	attempt      int
	startedAt    time.Time
	freshAllowed bool
}

// WithResumeSignal sets [ResumeContext.Signal] for the about-to-run
// execution. Defaults to "manual".
func WithResumeSignal(s string) LoadAndResumeOption {
	return func(o *loadAndResumeOpts) {
		if s != "" {
			o.signal = s
		}
	}
}

// WithResumeAttempt sets [ResumeContext.Attempt]. The default of 1
// is correct on fresh runs; supervisors implementing retry loops
// should bump this for each re-attempt.
func WithResumeAttempt(n int) LoadAndResumeOption {
	return func(o *loadAndResumeOpts) {
		if n > 0 {
			o.attempt = n
		}
	}
}

// WithResumeStartedAt sets [ResumeContext.StartedAt]. Default is
// time.Now() at the moment LoadAndResume is invoked. Pass an
// earlier timestamp when continuing a long-running run so dashboard
// "total wall time" remains accurate.
func WithResumeStartedAt(t time.Time) LoadAndResumeOption {
	return func(o *loadAndResumeOpts) {
		if !t.IsZero() {
			o.startedAt = t
		}
	}
}

// WithFreshStartAllowed controls behaviour when the store has no
// checkpoint for run.RunID. Default true — execute fresh. Pass false
// to require an existing checkpoint and surface
// errdefs.NotFound when none is present (useful for "resume only"
// admin commands).
func WithFreshStartAllowed(allowed bool) LoadAndResumeOption {
	return func(o *loadAndResumeOpts) {
		o.freshAllowed = allowed
	}
}

// LoadAndResume is the canonical host-side helper for "either
// continue the existing run or start fresh". It:
//
//  1. Loads the most recent checkpoint for run.RunID from store.
//  2. If a checkpoint exists, validates it against the engine's
//     [Resumer] (if implemented) — invalid checkpoints surface
//     immediately rather than after a partial Execute.
//  3. Populates run.ResumeFrom and threads a [ResumeContext] onto
//     ctx so the engine, observers and middleware see consistent
//     replay metadata.
//  4. Calls eng.Execute and returns its result.
//
// store may be nil; that is treated as "no checkpoints persisted"
// and is equivalent to a fresh-start with WithFreshStartAllowed
// honoured. board is the bootstrap board used on fresh starts and
// when the engine wishes to keep the host-supplied initial state
// (the engine itself decides whether to override with
// ResumeFrom.Board per the [Run.ResumeFrom] contract).
//
// LoadAndResume is intentionally a one-shot helper, not a retry
// loop: a supervisor that wants exponential backoff or budget
// enforcement composes LoadAndResume with its own retry policy.
func LoadAndResume(
	ctx context.Context,
	eng Engine,
	host Host,
	store CheckpointStore,
	run Run,
	board *Board,
	opts ...LoadAndResumeOption,
) (*Board, error) {
	if eng == nil {
		return board, errors.New("agent.LoadAndResume: nil engine")
	}
	if run.RunID == "" {
		return board, errdefs.Validation(errors.New("agent.LoadAndResume: Run.RunID is required"))
	}

	o := loadAndResumeOpts{signal: "manual", attempt: 1, freshAllowed: true}
	for _, fn := range opts {
		fn(&o)
	}
	if o.startedAt.IsZero() {
		o.startedAt = time.Now()
	}

	var cp *Checkpoint
	if store != nil {
		loaded, err := store.Load(ctx, run.RunID)
		if err != nil {
			return board, fmt.Errorf("agent.LoadAndResume: load %s: %w", run.RunID, err)
		}
		cp = loaded
	}

	if cp == nil {
		if !o.freshAllowed {
			return board, errdefs.NotFound(fmt.Errorf("agent.LoadAndResume: no checkpoint for run %s", run.RunID))
		}
		// Fresh start path. Still populate ResumeContext so
		// observers can record the attempt count uniformly.
		rc := ResumeContext{
			Attempt:   o.attempt,
			StartedAt: o.startedAt,
			Signal:    o.signal,
		}
		return eng.Execute(WithResumeContext(ctx, rc), run, host, board)
	}

	// Resume path — verify the engine recognises the checkpoint
	// before launching Execute. Engines that do not implement
	// Resumer skip the probe and rely on their own Execute-time
	// handling per the [Run.ResumeFrom] contract.
	if cp.ExecID != "" && cp.ExecID != run.RunID {
		return board, errdefs.Validation(fmt.Errorf(
			"agent.LoadAndResume: checkpoint exec_id %q does not match run %q",
			cp.ExecID, run.RunID,
		))
	}
	if r, ok := AsResumer(eng); ok {
		if err := r.CanResume(*cp); err != nil {
			return board, fmt.Errorf("agent.LoadAndResume: CanResume: %w", err)
		}
	}

	run.ResumeFrom = cp
	// Prefer the checkpoint's OriginalStartedAt so dashboards see
	// one continuous wall-clock run across resume boundaries. Fall
	// back to the caller-supplied o.startedAt for older checkpoints
	// produced before that field existed (zero time means "not
	// recorded" per Checkpoint.OriginalStartedAt godoc).
	startedAt := cp.OriginalStartedAt
	if startedAt.IsZero() {
		startedAt = o.startedAt
	}
	rc := ResumeContext{
		// First resume after a fresh attempt → 2; honour
		// caller-supplied attempt so retry loops can override.
		Attempt:      max(o.attempt, 2),
		StartedAt:    startedAt,
		Signal:       o.signal,
		CheckpointAt: cp.Timestamp,
	}
	return eng.Execute(WithResumeContext(ctx, rc), run, host, board)
}
