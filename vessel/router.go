package vessel

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/vessel/spec"

	otellog "go.opentelemetry.io/otel/log"
)

// agentEntry is the assembled per-agent runtime view the Captain
// holds for every spec.Agents entry. It bundles the agent.Agent
// value, its engine, the seeder + observers wired for this entry,
// and (for sidecars) the bus subscription token so Stop can tear
// the listener down.
//
// Splitting this from Captain itself keeps the multi-agent dispatch
// logic local: Submit looks the entry up by name and the rest of
// the lifecycle code does not have to special-case sidecar vs
// foreground agents.
type agentEntry struct {
	spec      spec.Agent
	agent     agent.Agent
	engine    engine.Engine
	seeder    agent.BoardSeeder
	observers []agent.Observer
	deciders  []agent.Decider

	// sidecar fields — non-nil only when spec.Sidecar is true.
	subscription event.Subscription
	subscribeCtx context.CancelFunc
}

// dispatch runs one [agent.Run] for entry against req, threading
// observers / deciders the Captain has accumulated for this entry.
// Returns the agent.Result the engine produced and the (possibly
// nil) error agent.Run returned.
//
// dispatch is the single funnel for both Submit (foreground) and
// the sidecar bus loop (background); keeping it in one spot
// guarantees observers + history wiring + telemetry semantics stay
// identical regardless of trigger type.
func (c *Captain) dispatch(ctx context.Context, entry *agentEntry, req agent.Request, extraOpts ...agent.RunOption) (*agent.Result, error) {
	// Surface the dispatcher's ContextID through ctx so the
	// vessel-aware kanban_submit tool can route the eventual
	// callback back to the same conversation. ctx is the only
	// signal the tool layer has — agent.Request is consumed by
	// agent.Run before the tool sees anything.
	if req.ContextID != "" {
		ctx = withContextID(ctx, req.ContextID)
	}
	// Also stamp the Captain pointer + the per-Run dispatcher
	// name. With these in ctx the vessel-aware kanban tools are
	// stateless: a single tool instance in the daemon-shared
	// tool.Registry serves every Captain × Dispatcher
	// combination, eliminating the registry-key collision that
	// pinning identity in struct fields would otherwise cause.
	ctx = withCaptain(ctx, c)
	ctx = withDispatcher(ctx, entry.spec.Name)
	opts := []agent.RunOption{
		agent.WithEngineHost(c.host),
	}
	if entry.seeder != nil {
		opts = append(opts, agent.WithBoardSeed(entry.seeder))
	}
	for _, o := range entry.observers {
		opts = append(opts, agent.WithObserver(o))
	}
	for _, d := range entry.deciders {
		opts = append(opts, agent.WithDecider(d))
	}
	for _, o := range c.globalObservers {
		opts = append(opts, agent.WithObserver(o))
	}
	for _, d := range c.globalDeciders {
		opts = append(opts, agent.WithDecider(d))
	}
	// extraOpts come last so callers like Captain.Resume can
	// override defaults (e.g. inject WithResumeFrom). agent.Run
	// applies options in order; later wins for fields like
	// resumeFrom.
	opts = append(opts, extraOpts...)
	return agent.Run(ctx, entry.agent, entry.engine, req, opts...)
}

// startSidecar registers entry as a bus subscriber. Every envelope
// matching entry.spec.SubscribeTo triggers one dispatch with a
// synthetic agent.Request whose Message body is empty (sidecars
// observe state, they do not receive chat turns) and whose Inputs
// carries the raw envelope under the well-known key "envelope" so
// custom engines can read the trigger.
//
// Errors from Subscribe (invalid pattern, closed bus) are surfaced
// to the caller; ctx scopes the subscription to the vessel
// lifecycle (Captain.rootCtx) so Stop tears it down automatically.
func (c *Captain) startSidecar(entry *agentEntry) error {
	if c.bus == nil {
		return errdefs.Validationf("vessel: sidecar agent %q requires a bus (none configured)", entry.spec.Name)
	}
	pattern := event.Pattern(entry.spec.SubscribeTo)
	if err := pattern.Validate(); err != nil {
		return errdefs.Validationf("vessel: sidecar %q SubscribeTo %q invalid: %v", entry.spec.Name, entry.spec.SubscribeTo, err)
	}
	subCtx, cancel := context.WithCancel(c.rootCtx)
	sub, err := c.bus.Subscribe(subCtx, pattern)
	if err != nil {
		cancel()
		return errdefs.Internalf("vessel: sidecar %q subscribe %q: %v", entry.spec.Name, entry.spec.SubscribeTo, err)
	}
	entry.subscription = sub
	entry.subscribeCtx = cancel

	c.inflight.Add(1)
	go c.runSidecarLoop(entry)
	return nil
}

// runSidecarLoop drains the sidecar's subscription channel and
// dispatches one Run per envelope. The loop exits when the
// subscription channel closes (rootCtx cancellation, bus close,
// caller-initiated unsubscribe) — at which point the inflight
// WaitGroup is released so Drain / Stop can complete.
//
// Failed dispatches are logged at warn level and the loop continues:
// a misbehaving sidecar must not bring down the main agents.
func (c *Captain) runSidecarLoop(entry *agentEntry) {
	defer c.inflight.Done()
	for env := range entry.subscription.C() {
		// Sidecar runs share the rootCtx — they exit on Stop /
		// Drain along with everything else. Per-envelope cancel
		// would let one slow sidecar starve concurrent triggers,
		// which is the wrong default for observability sidecars.
		//
		// ContextID is intentionally LEFT EMPTY: the per-trigger
		// envelope.RunID would otherwise become a transient
		// "conversation" id, scattering the sidecar's history into
		// one isolated thread per envelope. Sidecars should not
		// participate in the dispatcher-history scheme; if a
		// caller needs sidecar-scoped persistence they can attach
		// a custom Observer that derives a stable scope from the
		// envelope. buildEntries also rejects HistoryAccess=ReadWrite
		// on sidecars so this matches the static guard below.
		req := agent.Request{
			Message: model.Message{}, // empty; sidecars read envelope via Inputs
			Inputs:  map[string]any{"envelope": env},
		}
		runCtx, release, err := c.gate.acquire(c.rootCtx)
		if err != nil {
			telemetry.Warn(c.rootCtx, "vessel: sidecar admission denied",
				otellog.String("vessel_id", c.vs.ID),
				otellog.String("agent", entry.spec.Name),
				otellog.String("error", err.Error()))
			continue
		}
		_, runErr := c.dispatch(runCtx, entry, req)
		release()
		if runErr != nil {
			telemetry.Warn(c.rootCtx, "vessel: sidecar dispatch failed",
				otellog.String("vessel_id", c.vs.ID),
				otellog.String("agent", entry.spec.Name),
				otellog.String("subject", string(env.Subject)),
				otellog.String("error", runErr.Error()))
		}
	}
}

// stopSidecars cancels every sidecar subscription. Called from the
// Captain's teardown path; the runSidecarLoop goroutines exit on
// their own once the subscription channel closes, and the inflight
// WaitGroup tracking takes care of the wait.
func (c *Captain) stopSidecars() {
	for _, e := range c.entries {
		if e.subscribeCtx != nil {
			e.subscribeCtx()
		}
	}
}

// buildEntries assembles one agentEntry per spec.Agents entry.
// Called from New so all per-agent wiring happens up-front; Submit
// then becomes a fast map lookup.
//
// Dependency resolution order (matters for diagnostics):
//
//  1. EngineFactory(spec, deps) — fail fast with the per-agent name
//     in the error so callers can pinpoint which agent broke.
//  2. Seeder + Observers — assembled from the resolved history
//     (when set) so the dispatch path stays branch-free.
//  3. Sidecar subscription validation only — actual Subscribe
//     happens in Launch, when rootCtx exists.
func buildEntries(vs spec.Spec, factory EngineFactory, deps Deps, hist history.History) (map[string]*agentEntry, []*agentEntry, error) {
	entries := make(map[string]*agentEntry, len(vs.Agents))
	ordered := make([]*agentEntry, 0, len(vs.Agents))
	for i := range vs.Agents {
		aspec := vs.Agents[i]
		// Apply the same Tools augmentation the agent.Agent will
		// see (kanban auto-injection for Dispatchers) BEFORE
		// calling the factory. Engine factories that honour the
		// allow-list need it to match the runtime allow-list, not
		// the raw user-declared subset.
		augmented := aspec
		augmented.Tools = augmentedAgentTools(vs, aspec)
		eng, err := factory(augmented, deps)
		if err != nil {
			return nil, nil, errdefs.Internalf("vessel: engine factory for agent %q: %v", aspec.Name, err)
		}
		if eng == nil {
			return nil, nil, errdefs.Internalf("vessel: engine factory for agent %q returned nil engine", aspec.Name)
		}

		ag := buildAgentValue(vs.ID, augmented)
		entry := &agentEntry{spec: aspec, agent: ag, engine: eng}

		access := resolveHistoryAccess(hist != nil, aspec)
		// Sidecars MUST NOT have ReadWrite history. The sidecar
		// loop dispatches with an empty ContextID (per-envelope
		// runIDs would scatter writes across one orphan thread per
		// trigger), so a ReadWrite sidecar would either silently
		// drop every append (when ContextID is empty) or pollute
		// transcripts that were never theirs. Reject the spec at
		// New time rather than misbehave at runtime.
		if aspec.Sidecar && access == spec.HistoryAccessReadWrite {
			return nil, nil, errdefs.Validationf("vessel: sidecar agent %q cannot declare HistoryAccess=ReadWrite — sidecars run with empty ContextID and must use ReadOnly or None", aspec.Name)
		}
		if hist != nil && access != spec.HistoryAccessNone {
			entry.seeder = historySeeder{store: hist, access: access}
			if access == spec.HistoryAccessReadWrite {
				entry.observers = append(entry.observers, historyAppender{store: hist})
			}
		}

		entries[aspec.Name] = entry
		ordered = append(ordered, entry)
	}
	return entries, ordered, nil
}

// resolveHistoryAccess applies the default-by-context rule: when
// the spec declares no history, every agent is HistoryAccessNone.
// Otherwise an unset access defaults to ReadWrite.
func resolveHistoryAccess(hasHistory bool, aspec spec.Agent) spec.HistoryAccess {
	if !hasHistory {
		return spec.HistoryAccessNone
	}
	if aspec.HistoryAccess == "" {
		return spec.HistoryAccessReadWrite
	}
	return aspec.HistoryAccess
}

// augmentedAgentTools returns the agent's effective tool allow-list
// after applying vessel-runtime augmentations: Dispatcher agents
// receive the auto-injected kanban_submit / task_context ids when
// the vessel has Kanban enabled.
//
// Centralising the rule means buildEntries (passing tools to the
// factory) and buildAgentValue (populating agent.Agent.Tools) stay
// in sync: there is exactly one place to change when the
// augmentation rules grow.
func augmentedAgentTools(vs spec.Spec, aspec spec.Agent) []string {
	tools := append([]string(nil), aspec.Tools...)
	if aspec.Dispatcher && vs.Kanban != nil {
		tools = appendIfMissing(tools, "kanban_submit", "task_context")
	}
	return tools
}

// buildAgentValue produces the [agent.Agent] value the Captain
// hands to agent.Run. The id combines vessel id and per-agent name
// so multi-agent vessels keep their telemetry attributes distinct.
// Tools is taken verbatim from aspec — callers MUST pass the
// augmented spec.Agent (typically built via augmentedAgentTools).
func buildAgentValue(vesselID string, aspec spec.Agent) agent.Agent {
	a := agent.Agent{
		ID:    vesselID + "/" + aspec.Name,
		Tools: append([]string(nil), aspec.Tools...),
	}
	if card, ok := aspec.Card.(agent.AgentCard); ok {
		a.Card = card
	}
	return a
}

// appendIfMissing appends each id to dst only when not already
// present. Used to add Dispatcher tool ids without duplicating any
// the spec already declared explicitly.
func appendIfMissing(dst []string, ids ...string) []string {
	have := make(map[string]struct{}, len(dst))
	for _, id := range dst {
		have[id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := have[id]; ok {
			continue
		}
		dst = append(dst, id)
		have[id] = struct{}{}
	}
	return dst
}
