package vessel

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel/spec"

	otellog "go.opentelemetry.io/otel/log"
)

// chainDepthMetaKey is the well-known TaskOptions.Meta key the
// vessel-aware submit tool uses to smuggle the producer-chain depth
// from the dispatcher's tool call to the [captainExecutor] consumer
// side. Hidden under the "__vessel" prefix so the LLM never sees it
// in tool schemas and so user-supplied Meta keys cannot collide.
const chainDepthMetaKey = "__vessel_chain_depth"

// defaultMaxProducerChain is used when [spec.Kanban.MaxProducerChain]
// is unset. 8 is deep enough for legitimate Plan→Research→Verify
// chains and shallow enough that runaway loops fail fast in tests.
const defaultMaxProducerChain = 8

// defaultCallbackMaxSummary mirrors the prefix length sdk/kanban
// truncates ResultPayload.Output to inside BuildCallbackQuery. The
// vessel layer re-applies it explicitly so callers reading the
// vesselspec docs do not have to chase the kanban package for the
// number.
const defaultCallbackMaxSummary = 200

// kanbanRuntime is the resolved Kanban subsystem the Captain owns
// for the lifetime of one Vessel. It bundles the kanban.Kanban
// instance, the wrapped tool implementations, and the per-card
// metadata the callback bridge needs to route the result back to
// the correct dispatcher.
//
// Constructed in [Captain.New] when spec.Kanban != nil and torn
// down in [Captain.finalize]; nil when Kanban is disabled.
type kanbanRuntime struct {
	cfg    spec.Kanban
	board  *kanban.Board
	kanban *kanban.Kanban

	// dispMu guards dispCtx; recordOrigin runs on the dispatcher's
	// engine goroutine and lookup/consume run on the kanban
	// executor + callback bridge goroutines, so the map is shared
	// across at least three concurrent writers/readers.
	dispMu  sync.Mutex
	dispCtx map[string]dispatchOrigin
}

// dispatchOrigin records who issued a kanban_submit so the callback
// bridge can append the [Task Callback] message onto the correct
// agent's history and trigger their next turn.
type dispatchOrigin struct {
	dispatcherName string
	contextID      string
	depth          int
}

// withChainDepth scopes a context with the producer-chain depth the
// next nested submit (if any) should observe. Reading happens via
// [chainDepthFrom]; the key is package-private so external packages
// cannot accidentally fake the depth.
type chainDepthCtxKey struct{}

func withChainDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, chainDepthCtxKey{}, depth)
}

func chainDepthFrom(ctx context.Context) int {
	if d, ok := ctx.Value(chainDepthCtxKey{}).(int); ok {
		return d
	}
	return 0
}

// captainExecutor is the [kanban.AgentExecutor] implementation that
// glues the Kanban board to the Vessel's agent dispatch path. The
// kanban.Kanban subsystem invokes ExecuteTask in a fresh goroutine
// per Submit; this implementation looks up the target agentEntry,
// claims the card, dispatches the agent through [Captain.dispatch]
// (which threads observers / history / sandbox identically to a
// foreground Submit), and finally calls Done / Fail on the board so
// the dispatcher's callback message is generated.
type captainExecutor struct {
	c *Captain
}

// ExecuteTask satisfies kanban.AgentExecutor. The flow:
//
//  1. Reject when the Captain is no longer accepting work — Stop /
//     Drain set the phase below PhaseRunning, in which case any
//     newly-spawned card is failed immediately so the dispatcher
//     observes a callback rather than a phantom in-flight task.
//  2. Resolve the depth carried in card.Meta and bail with a clear
//     error when it would exceed the configured cap.
//  3. Look up the target agentEntry; missing target fails the card
//     so the dispatcher's callback says "no such agent".
//  4. Claim → dispatch → Done/Fail.
//
// Errors returned to kanban are nil even on failure paths — kanban
// already records its own bookkeeping when Done/Fail is called, and
// returning a non-nil error would cause kanban to call Fail a second
// time. The runtime distinction we owe upstream is the Card status,
// not a Go error.
//
// The corresponding inflight Add(1) is performed by [vesselSubmitTool.Execute]
// BEFORE kanban.Submit returns, so the WaitGroup counter is already
// non-zero when this goroutine starts. Defer Done() here so Drain /
// Stop wait for kanban-spawned dispatches the same way they wait
// for foreground Submits.
func (x *captainExecutor) ExecuteTask(ctx context.Context, _ string, targetAgentID string, card *kanban.Card, query string, inputs map[string]any) error {
	defer x.c.inflight.Done()
	if !x.c.Phase().AcceptsRequests() {
		x.failCard(card, "vessel not accepting work in phase "+string(x.c.Phase()))
		return nil
	}

	depth := readChainDepth(card.Meta)

	target, ok := x.c.entries[targetAgentID]
	if !ok {
		x.failCard(card, fmt.Sprintf("no agent named %q in vessel %q", targetAgentID, x.c.vs.ID))
		return nil
	}

	cap := chainCapForAgent(x.c.vs, target.spec)
	if depth > cap {
		x.failCard(card, fmt.Sprintf("producer chain depth %d exceeds cap %d", depth, cap))
		return nil
	}

	x.c.kanban.kanban.Board().Claim(card.ID, x.c.vs.ID+"/"+targetAgentID)

	// Build the Request from the kanban payload. ContextID points
	// the seeded transcript at the dispatcher's conversation so
	// the worker sees the caller's prior turns; the dispatcher
	// would otherwise be unable to share context without manually
	// piping it through Inputs.
	dispatcherName, dispatcherCtxID := x.c.kanban.lookupOrigin(card.ID)
	req := agent.Request{
		ContextID: dispatcherCtxID,
		Message:   model.NewTextMessage(model.RoleUser, query),
		Inputs:    inputs,
	}

	// Set the chain depth for any nested submit the worker may
	// issue: depth+1 because the worker is one level deeper than
	// the producer that put it here.
	childCtx := withChainDepth(ctx, depth+1)

	res, runErr := x.c.dispatch(childCtx, target, req)
	if runErr != nil {
		x.failCard(card, runErr.Error())
		return nil
	}
	// agent.Run signals run-level failure via res.Status, not via
	// the Go error return — translate non-completed terminals into
	// the matching kanban transition so the dispatcher's callback
	// reflects what actually happened.
	if res != nil && res.Status != agent.StatusCompleted {
		reason := "agent ended with status " + string(res.Status)
		if res.Err != nil {
			reason = res.Err.Error()
		}
		x.failCard(card, reason)
		return nil
	}

	out := lastAssistantText(res)
	x.c.kanban.kanban.Board().Done(card.ID, kanban.ResultPayload{Output: out})
	telemetry.Debug(ctx, "vessel: kanban task completed",
		otellog.String("vessel_id", x.c.vs.ID),
		otellog.String("card_id", card.ID),
		otellog.String("dispatcher", dispatcherName),
		otellog.String("target_agent", targetAgentID),
		otellog.Int("output_len", len(out)))
	return nil
}

// failCard publishes a Fail transition on the board and is the
// single place captainExecutor reports a non-success outcome.
// Centralising it keeps the per-branch error messages consistent.
func (x *captainExecutor) failCard(card *kanban.Card, reason string) {
	if x.c.kanban != nil && x.c.kanban.kanban != nil {
		x.c.kanban.kanban.Board().Fail(card.ID, reason)
	}
}

// lastAssistantText pulls the most recent assistant-role message
// from the agent.Result and returns its concatenated text. Empty
// string when the worker produced nothing — the callback will then
// say "Status: completed / Summary:" with an empty summary, which
// is the honest signal for a completion that yielded no text.
func lastAssistantText(res *agent.Result) string {
	if res == nil {
		return ""
	}
	for i := len(res.Messages) - 1; i >= 0; i-- {
		if res.Messages[i].Role == model.RoleAssistant {
			return res.Messages[i].Content()
		}
	}
	return ""
}

// chainCapForAgent picks the smaller of the per-agent ProducerChain
// and the global Spec.Kanban.MaxProducerChain. 0 means "use default"
// at both levels, so the resolved cap is always positive.
func chainCapForAgent(vs spec.Spec, agentSpec spec.Agent) int {
	global := vs.Kanban.MaxProducerChain
	if global <= 0 {
		global = defaultMaxProducerChain
	}
	per := agentSpec.ProducerChain
	if per <= 0 {
		return global
	}
	if per < global {
		return per
	}
	return global
}

// readChainDepth parses the depth Meta entry the vessel-aware submit
// tool stamped on the card. Defaults to 0 when missing or malformed
// so foreground submits (which never go through the tool) work the
// same as a freshly-parsed depth=0.
func readChainDepth(meta map[string]string) int {
	if meta == nil {
		return 0
	}
	v, ok := meta[chainDepthMetaKey]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// vesselSubmitTool is the wrapper around the LLM-facing
// kanban_submit tool. Compared to kanban.SubmitTool it adds:
//
//   - producer-chain bookkeeping (reads depth from ctx, stamps
//     depth+1 onto TaskOptions.Meta, rejects with a tool-level
//     error before submission when the cap is exceeded so the
//     dispatcher's LLM gets immediate feedback rather than a
//     callback failure);
//   - dispatcher origin tracking (records the cardID → dispatcher
//     mapping for the callback bridge to consume);
//   - per-Dispatcher target validation (defensive — the kanban
//     subsystem also validates, this just produces a sharper error
//     message that names the actual dispatcher).
//
// The tool is intentionally STATELESS — its identity (which Captain
// it serves, which Dispatcher invoked it) is resolved from ctx
// values that [Captain.dispatch] stamps before agent.Run begins.
// That keeps the daemon-shared tool.Registry safe in the face of
// multi-vessel + multi-dispatcher fleets: every wrapper instance
// is functionally identical, so registry-name collisions across
// captains and dispatchers are harmless.
type vesselSubmitTool struct{}

func (vesselSubmitTool) Definition() model.ToolDefinition {
	return tool.DefineSchema("kanban_submit",
		"Dispatch a task to another agent in this vessel. Returns a card_id immediately; the target agent runs in the background and the result is delivered as a [Task Callback] message on your next turn.",
		tool.Property("target_agent_id", "string", "Target agent name (one of the agents declared in the vessel)"),
		tool.Property("query", "string", "Task instruction for the target agent"),
		tool.Property("user_query", "string", "Original user request that triggered this dispatch"),
		tool.Property("dispatch_note", "string", "Brief note explaining why you delegated; helps you summarize the result for the user"),
	).Required("target_agent_id", "query").Build()
}

func (vesselSubmitTool) Execute(ctx context.Context, arguments string) (string, error) {
	c := captainFromCtx(ctx)
	if c == nil {
		return "", errdefs.NotAvailablef("kanban_submit: invoked outside a vessel dispatch ctx")
	}
	dispatcherName := dispatcherFromCtx(ctx)
	if dispatcherName == "" {
		return "", errdefs.NotAvailablef("kanban_submit: dispatcher identity not present in ctx")
	}
	rt := c.kanban
	if rt == nil || rt.kanban == nil {
		return "", errdefs.NotAvailablef("kanban_submit: vessel %q has no Kanban subsystem", c.vs.ID)
	}

	var args struct {
		TargetAgentID string `json:"target_agent_id"`
		Query         string `json:"query"`
		UserQuery     string `json:"user_query"`
		DispatchNote  string `json:"dispatch_note"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("kanban_submit: invalid arguments: %w", err)
	}

	// Reject self-dispatch up-front: useful guardrail and a clearer
	// error than the generic "no such agent" the executor would
	// emit if the dispatcher mistyped its own name.
	if args.TargetAgentID == dispatcherName {
		return "", errdefs.Validationf("kanban_submit: agent %q cannot dispatch to itself", dispatcherName)
	}
	target, ok := c.entries[args.TargetAgentID]
	if !ok {
		return "", errdefs.NotFoundf("kanban_submit: no agent named %q in vessel", args.TargetAgentID)
	}
	if target.spec.Sidecar {
		return "", errdefs.Conflictf("kanban_submit: agent %q is a sidecar; trigger via bus, not Submit", args.TargetAgentID)
	}

	depth := chainDepthFrom(ctx)
	dispatcher, _ := c.vs.Agent(dispatcherName)
	if dispatcher == nil {
		// Unreachable in practice: dispatcher names come from
		// agentEntry.spec.Name which only exists when the spec
		// declared the agent. Guard anyway because vesselspec
		// mutation is not enforced.
		return "", errdefs.Internalf("kanban_submit: dispatcher %q absent from spec", dispatcherName)
	}
	chainCap := chainCapForAgent(c.vs, *dispatcher)
	if depth+1 > chainCap {
		return "", errdefs.Conflictf("kanban_submit: producer chain depth %d exceeds cap %d for agent %q", depth+1, chainCap, dispatcherName)
	}

	// Look up the dispatcher's current ContextID so we can route
	// the callback message back to the correct conversation. Best
	// effort: a Dispatcher submitted from a non-history vessel
	// (or a Sidecar Dispatcher with no ContextID) gets an empty
	// ContextID — the callback bridge then skips the history
	// append and only re-Submits.
	contextID := contextIDFromCtx(ctx)

	// Reserve an inflight slot BEFORE handing the task to kanban
	// so Drain / Stop wait for the executor goroutine kanban will
	// spawn. captainExecutor.ExecuteTask defers Done(); if Submit
	// rejects the task we balance the Add with an immediate Done
	// so the counter never strands.
	c.inflight.Add(1)
	cardID, err := rt.kanban.Submit(ctx, kanban.TaskOptions{
		TargetAgentID: args.TargetAgentID,
		Query:         args.Query,
		UserQuery:     args.UserQuery,
		DispatchNote:  args.DispatchNote,
		Meta: map[string]string{
			chainDepthMetaKey: strconv.Itoa(depth + 1),
		},
	})
	if err != nil {
		c.inflight.Done()
		return "", err
	}

	rt.recordOrigin(cardID, dispatchOrigin{
		dispatcherName: dispatcherName,
		contextID:      contextID,
		depth:          depth,
	})

	out, _ := json.Marshal(map[string]string{
		"card_id":         cardID,
		"target_agent_id": args.TargetAgentID,
		"status":          "submitted",
		"message":         "Task accepted. The result will arrive as a [Task Callback] user message on your next turn.",
	})
	return string(out), nil
}

// vesselTaskContextTool is a thin wrapper around kanban.TaskContextTool
// that resolves the Kanban instance through ctx rather than through
// a struct field — same rationale as vesselSubmitTool: a stateless
// wrapper makes registry-key collisions across captains harmless.
type vesselTaskContextTool struct{}

func (vesselTaskContextTool) Definition() model.ToolDefinition {
	return tool.DefineSchema("task_context",
		"Retrieve the full context of a previously dispatched task — the original user request, your dispatch note, the task instruction, and the execution result. Use this when you receive a [Task Callback] and need details beyond the truncated summary.",
		tool.Property("card_id", "string", "card_id from the [Task Callback] message or from the kanban_submit response"),
	).Required("card_id").Build()
}

func (vesselTaskContextTool) Execute(ctx context.Context, arguments string) (string, error) {
	c := captainFromCtx(ctx)
	if c == nil {
		return "", errdefs.NotAvailablef("task_context: invoked outside a vessel dispatch ctx")
	}
	rt := c.kanban
	if rt == nil || rt.kanban == nil {
		return "", errdefs.NotAvailablef("task_context: vessel %q has no Kanban subsystem", c.vs.ID)
	}
	var args struct {
		CardID string `json:"card_id"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("task_context: invalid arguments: %w", err)
	}
	card, err := rt.kanban.GetCard(ctx, args.CardID)
	if err != nil {
		return "", err
	}
	return kanban.BuildTaskContext(card), nil
}

// recordOrigin is the captainExecutor's input — every vessel-aware
// kanban_submit call writes one entry so the callback bridge can
// look up "who dispatched this card" when the card terminates.
//
// The map grows monotonically for the lifetime of one Vessel; we
// drop entries inside [kanbanRuntime.lookupOrigin] right after they
// are consumed, which keeps the working set bounded by the count of
// in-flight dispatches.
func (rt *kanbanRuntime) recordOrigin(cardID string, origin dispatchOrigin) {
	rt.dispMu.Lock()
	defer rt.dispMu.Unlock()
	rt.dispCtx[cardID] = origin
}

// lookupOrigin returns the dispatchOrigin recorded by the submit
// tool. Missing entries (foreground Submit, kanban-internal cron
// triggers, cards re-loaded from a persistent board) yield zero
// values; callers must treat empty dispatcherName as "no dispatcher
// to call back" and route accordingly.
func (rt *kanbanRuntime) lookupOrigin(cardID string) (dispatcherName string, contextID string) {
	rt.dispMu.Lock()
	defer rt.dispMu.Unlock()
	o, ok := rt.dispCtx[cardID]
	if !ok {
		return "", ""
	}
	return o.dispatcherName, o.contextID
}

// consumeOrigin is the destructive variant the callback bridge uses
// after a card terminates: read the entry and remove it so the map
// does not grow unboundedly.
func (rt *kanbanRuntime) consumeOrigin(cardID string) (dispatchOrigin, bool) {
	rt.dispMu.Lock()
	defer rt.dispMu.Unlock()
	o, ok := rt.dispCtx[cardID]
	if !ok {
		return dispatchOrigin{}, false
	}
	delete(rt.dispCtx, cardID)
	return o, true
}

// contextIDCtxKey scopes the dispatcher's [agent.Request.ContextID]
// in the per-Run ctx so the submit tool can recover it without
// having to reach back into the agent.Request value (which the
// Tool.Execute signature does not expose).
type contextIDCtxKey struct{}

func withContextID(ctx context.Context, contextID string) context.Context {
	if contextID == "" {
		return ctx
	}
	return context.WithValue(ctx, contextIDCtxKey{}, contextID)
}

func contextIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(contextIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// captainCtxKey + dispatcherCtxKey scope the active Captain pointer
// and the per-Run dispatcher name into the engine ctx so the
// vessel-aware kanban tools can stay stateless. [Captain.dispatch]
// sets both before invoking agent.Run; the tools read them in
// Execute. Keeping these private to the vessel package means
// external packages cannot fake either identity.
type captainCtxKey struct{}
type dispatcherCtxKey struct{}

func withCaptain(ctx context.Context, c *Captain) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, captainCtxKey{}, c)
}

func captainFromCtx(ctx context.Context) *Captain {
	if v, ok := ctx.Value(captainCtxKey{}).(*Captain); ok {
		return v
	}
	return nil
}

func withDispatcher(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, dispatcherCtxKey{}, name)
}

func dispatcherFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(dispatcherCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// startCallbackBridge wires the kanban board's terminal events to
// the dispatcher's history + a fresh agent.Run so the dispatcher's
// LLM observes the result on its next turn. Called from
// [Captain.Launch] and torn down by stopping the inflight loop when
// rootCtx is cancelled.
//
// Bridge logic:
//
//   - Subscribe to "kanban.card.>" — completed/failed/cancelled all
//     surface here, plus claim/submit which we filter out.
//   - For every terminal envelope, look up the dispatch origin. If
//     none, ignore (foreground Submit / Call gets the result via
//     Handle, no callback needed).
//   - Build a [Task Callback] message via kanban.BuildCallbackQuery,
//     append it as a user-role message to the dispatcher's history,
//     then re-Submit a fresh run on the dispatcher with that message
//     as the input — the LLM sees both the appended history and a
//     conversational prompt indicating "your dispatched task is back,
//     decide what to do".
//
// The bridge is fire-and-forget: a failure to append history or to
// Submit is logged but does not stop the loop.
func (c *Captain) startCallbackBridge() error {
	if c.kanban == nil {
		return nil
	}
	// Kanban events flow through the Board's own bus, not the
	// vessel bus — Board owns its bus internally and that is the
	// only place EventTaskCompleted / Failed / Cancelled show up.
	// Subscribing to c.bus would silently see nothing, so we go
	// straight to the source.
	kanbanBus := c.kanban.board.Bus()
	if kanbanBus == nil {
		return nil
	}
	pattern := event.Pattern("kanban.card.>")
	subCtx, cancel := context.WithCancel(c.rootCtx)
	sub, err := kanbanBus.Subscribe(subCtx, pattern)
	if err != nil {
		cancel()
		return errdefs.Internalf("vessel: kanban callback bridge subscribe: %v", err)
	}
	c.kanbanBridgeCancel = cancel
	c.kanbanBridgeWG.Add(1)
	go c.runCallbackBridge(sub)
	return nil
}

// runCallbackBridge is the bridge goroutine spawned by
// [startCallbackBridge]. Exits when the subscription channel closes
// (kanbanBridgeCancel fires or the bus is closed); decrements
// kanbanBridgeWG so [Captain.finalize] can wait for the bridge to
// drain its last in-flight callback before releasing resources.
//
// The bridge is NOT folded into Captain.inflight: Drain wants to
// observe inflight=0 (foreground + kanban work fully settled) BEFORE
// cancelling the bridge — so callbacks from the last kanban task
// still reach the dispatcher's history. Putting the bridge on its
// own WG is what makes that ordering possible.
func (c *Captain) runCallbackBridge(sub event.Subscription) {
	defer c.kanbanBridgeWG.Done()
	for env := range sub.C() {
		c.handleCallback(env)
	}
}

// handleCallback processes one envelope from the kanban bus. The
// terminal events we care about are:
//
//	kanban.card.<id>.task.completed
//	kanban.card.<id>.task.failed
//	kanban.card.<id>.task.cancelled
//
// Anything else is ignored; the wildcard subscription is broad on
// purpose so future kanban subjects do not require a vessel update.
func (c *Captain) handleCallback(env event.Envelope) {
	cardID, status, ok := parseCardTerminal(env)
	if !ok {
		return
	}
	origin, found := c.kanban.consumeOrigin(cardID)
	if !found {
		// Foreground Submit / Call: caller observes via Handle.
		return
	}
	dispatcher, ok := c.entries[origin.dispatcherName]
	if !ok {
		telemetry.Warn(c.rootCtx, "vessel: callback bridge cannot find dispatcher",
			otellog.String("vessel_id", c.vs.ID),
			otellog.String("dispatcher", origin.dispatcherName),
			otellog.String("card_id", cardID))
		return
	}

	card, err := c.kanban.kanban.GetCard(c.rootCtx, cardID)
	if err != nil {
		telemetry.Warn(c.rootCtx, "vessel: callback bridge cannot fetch card",
			otellog.String("vessel_id", c.vs.ID),
			otellog.String("card_id", cardID),
			otellog.String("error", err.Error()))
		return
	}

	result := resultPayloadFromCard(card, status)
	callbackText := kanban.BuildCallbackQuery(card, &result)
	callbackText = trimCallbackSummary(callbackText, c.kanban.cfg.CallbackMaxSummary)
	callbackMsg := model.NewTextMessage(model.RoleUser, callbackText)

	// Append onto the dispatcher's transcript so the next turn the
	// LLM observes its history grow with the callback. Best effort:
	// vessels without history (no spec.History or dispatcher
	// HistoryAccess=None) skip the append and rely on the Submit
	// below to deliver the callback inline as the user message.
	if c.history != nil && origin.contextID != "" {
		access := resolveHistoryAccess(c.history != nil, dispatcher.spec)
		if access == spec.HistoryAccessReadWrite {
			if appendErr := c.history.Append(c.rootCtx, origin.contextID, []model.Message{callbackMsg}); appendErr != nil {
				telemetry.Warn(c.rootCtx, "vessel: callback history append failed",
					otellog.String("vessel_id", c.vs.ID),
					otellog.String("dispatcher", origin.dispatcherName),
					otellog.String("error", appendErr.Error()))
			}
		}
	}

	// Re-Submit the dispatcher so its LLM gets a chance to react.
	// We tag the synthetic run id with the originating cardID so
	// telemetry can correlate the callback turn with the dispatch.
	req := agent.Request{
		ContextID: origin.contextID,
		Message:   callbackMsg,
		RunID:     "cb-" + cardID,
	}
	h, submitErr := c.Submit(c.rootCtx, origin.dispatcherName, req)
	if submitErr != nil {
		telemetry.Warn(c.rootCtx, "vessel: callback re-submit failed",
			otellog.String("vessel_id", c.vs.ID),
			otellog.String("dispatcher", origin.dispatcherName),
			otellog.String("card_id", cardID),
			otellog.String("error", submitErr.Error()))
		return
	}
	// We do not block on the handle here: the callback turn runs
	// like any other Submit and other observers/loggers can pick
	// it up via Logs / phase events.
	_ = h
}

// parseCardTerminal returns (cardID, status, true) when env is one
// of the three terminal kanban subjects we react to, and (_,_, false)
// otherwise. Status is the literal "completed" / "failed" /
// "cancelled" string; consumers use it to drive payload decoding.
//
// A terminal subject with an empty CardID header is reported as
// (_,_, false) AND surfaced via telemetry so the operator can
// notice when an upstream change to kanban.HeaderCardID drifts
// out from under the bridge — the previous silent-empty path made
// this kind of breakage invisible.
func parseCardTerminal(env event.Envelope) (string, string, bool) {
	subj := string(env.Subject)
	var status string
	switch {
	case hasSuffix(subj, ".task.completed"):
		status = "completed"
	case hasSuffix(subj, ".task.failed"):
		status = "failed"
	case hasSuffix(subj, ".task.cancelled"):
		status = "cancelled"
	default:
		return "", "", false
	}
	cardID := env.Headers[kanban.HeaderCardID]
	if cardID == "" {
		telemetry.Warn(context.Background(),
			"vessel: kanban terminal envelope missing card-id header",
			otellog.String("subject", subj),
			otellog.String("status", status))
		return "", "", false
	}
	return cardID, status, true
}

// hasSuffix is the strings.HasSuffix wrapper we use locally to keep
// the kanban.go imports lean (avoids pulling in strings just for
// this single call site shared by parseCardTerminal alone).
func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// resultPayloadFromCard reconstructs a [kanban.ResultPayload] from
// the terminal Card so [kanban.BuildCallbackQuery] can format the
// callback message identically regardless of which terminal status
// produced it. Empty Output is fine — the formatter handles it.
func resultPayloadFromCard(card *kanban.Card, status string) kanban.ResultPayload {
	switch status {
	case "completed":
		// After Done(), Card.Payload is map[string]any with the
		// original task fields plus "output".
		p := kanban.PayloadMap(card.Payload)
		out, _ := p["output"].(string)
		return kanban.ResultPayload{Output: out}
	case "failed":
		return kanban.ResultPayload{Error: card.Error}
	case "cancelled":
		return kanban.ResultPayload{Error: "cancelled: " + card.Error}
	}
	return kanban.ResultPayload{}
}

// trimCallbackSummary applies CallbackMaxSummary to the formatted
// callback text. We do it post-format because BuildCallbackQuery
// already truncates the Output prefix to 200 chars; the additional
// trim here gives callers explicit control over the total message
// length when they want a tighter budget than kanban's default.
//
// 0 means "use kanban default" → return the text unchanged so
// kanban's own truncation is the only one in play.
func trimCallbackSummary(text string, maxSummary int) string {
	if maxSummary <= 0 || maxSummary >= defaultCallbackMaxSummary {
		return text
	}
	return kanban.CompactCallbackForMemory(text, maxSummary)
}

// stopKanban tears down the kanbanRuntime. Called from
// [Captain.finalize]; safe to call when kanban is nil.
//
// Order of operations matters: cancel the bridge subscription
// (which causes the goroutine's range loop to exit), wait for
// the bridge goroutine to finish so kanbanBridgeWG drains, then
// stop the kanban subsystem. Reversing the bridge cancel + wait
// pair would let Stop fire while the bridge is still processing
// a final callback.
func (c *Captain) stopKanban() {
	if c.kanban == nil {
		return
	}
	if c.kanbanBridgeCancel != nil {
		c.kanbanBridgeCancel()
	}
	c.kanbanBridgeWG.Wait()
	c.kanban.kanban.Stop()
}

// buildKanbanRuntime constructs the per-Vessel kanbanRuntime shell:
// just the spec slice, the Board, and the cardID→origin map. The
// actual kanban.Kanban is left nil and assigned later by
// [installKanbanExecutor], which needs the surrounding Captain
// pointer to wire the [captainExecutor] via kanban.WithAgentExecutor.
//
// Returns nil when Kanban is disabled — the rest of the Captain then
// short-circuits every kanban code path off `c.kanban != nil`.
func buildKanbanRuntime(vs spec.Spec) *kanbanRuntime {
	if vs.Kanban == nil {
		return nil
	}
	return &kanbanRuntime{
		cfg:     *vs.Kanban,
		board:   kanban.NewBoard(vs.ID),
		dispCtx: make(map[string]dispatchOrigin),
	}
}

// installKanbanExecutor finishes the Kanban construction by building
// kanban.Kanban with the captainExecutor wired in. Called from
// [Captain.Launch] each time the Captain enters PhaseRunning so the
// kanban worker pool is scoped to the current rootCtx — Stop's
// rootCancel then propagates into kanban-spawned dispatch ctxs and
// the 5s WithStopTimeout becomes a fallback rather than the only
// cancellation path.
//
// Two-phase construction is unavoidable because kanban.New only
// accepts the executor through its WithAgentExecutor option, and the
// executor needs to dereference the Captain pointer for entry lookup
// and dispatch. It also has to be called from Launch (not New)
// because rootCtx is created in Launch — passing baseCtx (the
// caller-provided long-lived ctx) would leave kanban workers running
// past Stop until WithStopTimeout fired.
func installKanbanExecutor(c *Captain) {
	if c.kanban == nil {
		return
	}
	parentCtx := c.rootCtx
	if parentCtx == nil {
		parentCtx = c.baseCtx
	}
	c.kanban.kanban = kanban.New(parentCtx, c.kanban.board,
		kanban.WithConfig(kanban.KanbanConfig{
			MaxPendingTasks: c.kanban.cfg.MaxPendingTasks,
		}),
		kanban.WithStopTimeout(5*time.Second),
		kanban.WithAgentExecutor(&captainExecutor{c: c}),
	)
}

// registerDispatcherTools installs the kanban_submit / task_context
// tool wrappers into reg. Called from [Captain.New] when the spec
// declares Kanban with at least one Dispatcher.
//
// The wrappers are STATELESS (see vesselSubmitTool / vesselTaskContextTool
// — both resolve their Captain + Dispatcher from ctx values stamped
// by [Captain.dispatch]). Registering once therefore covers every
// Captain × Dispatcher combination; subsequent vessels in the same
// daemon-shared registry overwrite the entries with functionally
// identical instances and the runtime keeps working correctly.
//
// reg is the shared tool.Registry the Captain hands to EngineFactory
// via Deps.ToolRegistry; if the caller did not supply one, the
// Captain creates a fresh Registry so the tools always have a home.
func registerDispatcherTools(reg *tool.Registry, c *Captain) {
	if c.kanban == nil {
		return
	}
	hasDispatcher := false
	for _, e := range c.ordered {
		if e.spec.Dispatcher {
			hasDispatcher = true
			break
		}
	}
	if !hasDispatcher {
		return
	}
	reg.Register(vesselTaskContextTool{})
	reg.Register(vesselSubmitTool{})
}

// _ pulls the engine + history packages into the import set so the
// blank-import / aliasing comments stay visible to readers; the
// actual usage is via the history.History method calls and
// engine.MainChannel referenced elsewhere in the package.
var (
	_                 = engine.MainChannel
	_ history.History = nil
)
