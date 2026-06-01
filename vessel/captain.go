package vessel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel/spec"

	otellog "go.opentelemetry.io/otel/log"
)

// Captain is the in-process controller that owns a vessel's runtime
// state. One Captain per [spec.Spec]; multiple Captains can
// coexist in the same process (each vessel has its own bus, agents,
// and lifecycle).
//
// The zero value is not usable; construct via [New].
type Captain struct {
	vs      spec.Spec
	entries map[string]*agentEntry
	ordered []*agentEntry
	history history.History

	host     engine.Host
	bus      event.Bus
	busOwned bool
	gate     *admissionGate
	budget   *tokenBudget // nil when neither token cap is configured
	probes   *probeRunner

	// checkpointStore mirrors the store wired into the sandbox host
	// via [WithCheckpointStore]. The Captain keeps its own
	// reference so [Resume] can Load checkpoints without going
	// through the host (which only exposes the Save side via
	// engine.Checkpointer). nil when no store was configured —
	// Resume then surfaces NotAvailable, since the durability
	// promise the API is built on is missing.
	checkpointStore engine.CheckpointStore

	// sessionStore provisions a per-run workspace.Workspace view
	// for every Submit / Resume dispatch. nil when no store was
	// wired via [WithSessionStore]; in that case WorkspaceFromContext
	// returns (nil, false) inside every run.
	sessionStore SessionStore

	// kanban is the Kanban subsystem when spec.Kanban is non-nil;
	// otherwise nil (no agent-as-tool dispatch). The runtime owns
	// the board, kanban instance, and the cardID→dispatcher map
	// the callback bridge consumes.
	kanban             *kanbanRuntime
	kanbanBridgeCancel context.CancelFunc
	// kanbanBridgeWG tracks the callback bridge goroutine
	// independently of `inflight` so Drain can settle real work
	// (foreground Submit + kanban-spawned dispatch) first, then
	// tear the bridge down. Folding the bridge into `inflight`
	// would deadlock Drain: the bridge only exits when its
	// subscription channel closes, which happens on bridgeCancel —
	// but bridgeCancel is only called from finalize, which runs
	// AFTER inflight.Wait returns. Separating the two waits is
	// what lets Drain make progress.
	kanbanBridgeWG sync.WaitGroup

	// testRegistry is the tool.Registry the Captain installed the
	// auto-injected Kanban tools into. Exposed as a non-exported
	// field so kanban_test.go can assert installation without
	// vessel-internal package boundary tricks; production callers
	// receive the same registry through Deps.ToolRegistry.
	testRegistry *tool.Registry

	globalObservers []agent.Observer
	globalDeciders  []agent.Decider
	baseCtx         context.Context

	// rootCtx + rootCancel scope every in-flight run launched by
	// the Captain. Stop calls rootCancel to broadcast cancellation
	// to every goroutine without having to track them individually.
	rootCtx    context.Context
	rootCancel context.CancelFunc

	// inflight tracks Submit goroutines for Drain / Stop. Each
	// Submit goroutine increments before dispatch and decrements
	// via defer when agent.Run returns. Sidecar loops also share
	// this WaitGroup so a Drain waits for the bus subscription to
	// fully close.
	inflight sync.WaitGroup

	// mu guards phase transitions to keep them monotonic per the
	// state diagram in [Phase]. Reads happen via Phase() which
	// uses the atomic load underneath.
	mu    sync.Mutex
	phase atomic.Value // Phase

	// restartAttempts is the running tally of consecutive failure
	// cycles since the captain last had a "stable" stretch in
	// PhaseRunning. Bumped at the top of each restartLoop iteration
	// (one per re-Launch attempt); compared against
	// spec.Restart.MaxRestarts to drive the finalize-on-exhaust
	// path. Crucially it is shared across restartLoop spawns —
	// without that, probe-driven flap (which spawns a fresh
	// restartLoop per cycle) would never accumulate enough
	// "attempts" to trip MaxRestarts. Mu-protected; modified only
	// while holding c.mu.
	restartAttempts int

	// enteredRunningAt records the wall-clock time the captain
	// most recently transitioned INTO PhaseRunning. Used in
	// transitionPhaseLocked to decide whether the previous Running
	// stretch counted as "stable enough to forgive past failures":
	// when leaving Running after stableWindow has elapsed,
	// restartAttempts resets to 0. Mu-protected.
	enteredRunningAt time.Time
}

// restartStableWindow is how long the captain must stay in
// PhaseRunning between consecutive failures before its
// restart-attempts counter is forgiven. Modeled after the
// Kubernetes CrashLoopBackOff "stable run" heuristic; chosen
// generously so transient hiccups don't reset the counter while
// a genuinely-recovered vessel does.
//
// Stored as an atomic int64 of nanoseconds so the package's own
// tests can compress it for fast iteration without racing
// captain goroutines; production callers should not rely on it
// being mutable from outside the package.
var restartStableWindow atomic.Int64

func init() { restartStableWindow.Store(int64(30 * time.Second)) }

// stableWindow returns the current restart stable window. Always
// use this accessor instead of reading the var directly: race-free,
// and lets tests swap the value via overrideStableWindow.
func stableWindow() time.Duration { return time.Duration(restartStableWindow.Load()) }

// New constructs a Captain from spec. It validates the spec, applies
// options, builds the per-agent runtime view (engine, seeder,
// observers), but does NOT start anything — the Captain stays in
// PhasePending until Launch is called.
//
// Errors are classified via sdk/errdefs (Validation for spec /
// option issues, Internal for engine factory failures).
func New(vs spec.Spec, opts ...Option) (*Captain, error) {
	if err := vs.Validate(); err != nil {
		return nil, err
	}

	cfg := &config{baseCtx: context.Background()}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if cfg.engineFactory == nil {
		return nil, errdefs.Validationf("vessel: WithEngine or WithEngineFactory is required")
	}

	if vs.ID == "" {
		vs.ID = mintVesselID()
	}

	bus := cfg.bus
	owned := cfg.bus == nil
	if bus == nil {
		bus = event.NewMemoryBus()
	}

	hist, err := buildHistoryStore(vs, cfg.historyOverride)
	if err != nil {
		return nil, err
	}

	// Kanban subsystem (board + dispatch state) is built first so the
	// shared tool registry can pull the auto-injected SubmitTool /
	// TaskContextTool ids in the same Deps payload the EngineFactory
	// receives. The kanban.Kanban instance itself is wired below
	// once we have a Captain pointer for the executor closure.
	kbRuntime := buildKanbanRuntime(vs)

	// Resolve the tool registry: caller-supplied wins; otherwise we
	// own one so the auto-injected kanban tools always have a home.
	reg := cfg.toolRegistry
	if reg == nil && kbRuntime != nil {
		reg = tool.NewRegistry()
	}

	deps := Deps{
		ToolRegistry:    reg,
		LLMResolver:     cfg.llmResolver,
		Bus:             bus,
		History:         hist,
		CheckpointStore: cfg.checkpointStore,
	}
	entries, ordered, err := buildEntries(vs, cfg.engineFactory, deps, hist)
	if err != nil {
		return nil, err
	}

	gate := newAdmissionGate(vs.Resources.MaxConcurrentRuns, makeTimeoutFunc(vs.Resources.TurnTimeout))
	budget := newTokenBudget(vs.Resources.MaxTokensPerTurn, vs.Resources.MaxTokensPerHour)
	host := newSandboxHost(cfg.engineHost, bus, cfg.checkpointStore)

	c := &Captain{
		vs:              vs,
		entries:         entries,
		ordered:         ordered,
		history:         hist,
		host:            host,
		bus:             bus,
		busOwned:        owned,
		gate:            gate,
		budget:          budget,
		checkpointStore: cfg.checkpointStore,
		sessionStore:    cfg.sessionStore,
		kanban:          kbRuntime,
		globalObservers: cfg.observers,
		globalDeciders:  cfg.deciders,
		baseCtx:         cfg.baseCtx,
	}
	c.phase.Store(PhasePending)
	if vs.Probes != nil {
		// Wire any captain-introspecting probes BEFORE the runner
		// is constructed so the first probe tick sees a valid
		// reader; otherwise a TokenBudgetProbe declared in spec
		// would silently report Healthy=true on its first cycle.
		for _, pr := range vs.Probes.Liveness {
			if tbp, ok := pr.(*TokenBudgetProbe); ok {
				tbp.setBudget(budgetReaderFor(budget))
			}
		}
		c.probes = newProbeRunner(c, *vs.Probes)
	}

	// Two-phase Kanban setup: now that c exists, wire the executor
	// and register the per-Dispatcher tool wrappers into the shared
	// registry the EngineFactory will resolve from.
	if c.kanban != nil {
		// Tools register at New time (their wrappers are
		// stateless and ctx-resolved, see vessel/kanban.go), but
		// the kanban.Kanban worker pool is constructed in Launch
		// so it can be scoped to rootCtx — see installKanbanExecutor
		// for why baseCtx is wrong here.
		registerDispatcherTools(reg, c)
		c.testRegistry = reg
	}
	return c, nil
}

// makeTimeoutFunc returns the per-Run [timeoutFunc] strategy used by
// the admission gate. When d is zero the gate uses context.WithCancel
// so behaviour matches "no timeout".
func makeTimeoutFunc(d time.Duration) timeoutFunc {
	if d <= 0 {
		return nil
	}
	return func(parent context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(parent, d)
	}
}

// Phase reports the current lifecycle phase. Safe to call
// concurrently with Launch / Submit / Stop.
func (c *Captain) Phase() Phase { return c.phase.Load().(Phase) }

// ID returns the vessel identifier (caller-supplied or
// Captain-generated). Stable for the lifetime of the Captain.
func (c *Captain) ID() string { return c.vs.ID }

// Bus returns the event.Bus engine envelopes flow through. Useful
// for callers that want to attach their own subscribers (logs /
// metrics / SSE bridges) before Launch.
func (c *Captain) Bus() event.Bus { return c.bus }

// History returns the shared history.History the Captain assembled
// from spec.History (or the override passed via [WithHistory]). nil
// when the spec declares no history. Exposed so applications can
// inspect or share the transcript across vessels without a separate
// dependency injection pass.
func (c *Captain) History() history.History { return c.history }

// Launch transitions the Captain from PhasePending into PhaseRunning,
// subscribes every Sidecar agent to the bus, and starts the probe
// loop (when Probes is configured).
//
// Launch is idempotent on re-entry into PhaseRunning (returns nil
// without re-launching) but rejects launches from any other phase
// with errdefs.Conflict so callers cannot accidentally restart a
// stopped vessel — that is the [spec.Restart] loop's job.
func (c *Captain) Launch(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.Phase() {
	case PhasePending, PhaseFailed:
		// PhaseFailed → Pending → Running is the path the restart
		// loop takes; allow it explicitly so external callers can
		// also retry after a manual repair.
	case PhaseRunning:
		return nil
	default:
		return errdefs.Conflictf("vessel: cannot Launch from phase %q", c.Phase())
	}

	c.rootCtx, c.rootCancel = context.WithCancel(c.baseCtx)

	// Kanban worker pool is launched here (not in New) so its
	// internal ctx is rootCtx — Stop's rootCancel then cancels
	// in-flight kanban dispatches immediately instead of relying
	// on the 5s WithStopTimeout fallback.
	installKanbanExecutor(c)

	for _, entry := range c.ordered {
		if !entry.spec.Sidecar {
			continue
		}
		if err := c.startSidecar(entry); err != nil {
			// Roll back: cancel any sidecars already started, drop
			// the rootCtx, surface the error.
			c.stopSidecars()
			c.rootCancel()
			c.rootCtx, c.rootCancel = nil, nil
			return err
		}
	}

	if err := c.startCallbackBridge(); err != nil {
		c.stopSidecars()
		c.rootCancel()
		c.rootCtx, c.rootCancel = nil, nil
		return err
	}

	c.transitionPhaseLocked(PhaseRunning, "")
	c.probes.start()
	return nil
}

// Submit dispatches one [agent.Run] asynchronously. The returned
// Handle is the caller's receipt — call Handle.Wait to retrieve the
// result, or use [Logs] / [LogsForRun] to observe streaming output
// as it happens.
//
// agentName MUST match a non-Sidecar entry in spec.Agents. Sidecar
// agents reject foreground Submit so callers do not accidentally
// double-trigger an agent that is already wired to a bus pattern;
// invoke them via the bus instead.
//
// Submit refuses requests outside PhaseRunning with
// errdefs.NotAvailable so callers can drain gracefully without
// races.
func (c *Captain) Submit(ctx context.Context, agentName string, req agent.Request) (*Handle, error) {
	return c.submit(ctx, agentName, req, nil)
}

// Resume re-launches a previously interrupted (or persisted) run by
// loading its checkpoint from the wired [engine.CheckpointStore] and
// dispatching the original agent with engine.Run.ResumeFrom set.
//
// Lookup contract:
//
//   - The store MUST be configured via [WithCheckpointStore]; without
//     it Resume returns errdefs.NotAvailable (resume requires durable
//     state and there is none).
//
//   - runID MUST identify a previously-saved checkpoint
//     (cp.ExecID == runID). Missing checkpoints surface as
//     errdefs.NotFound.
//
//   - The checkpoint MUST carry the originating agent name in
//     cp.Attributes["vessel.agent_name"] (the sandbox host stamps
//     this on every Save). Older checkpoints without the field, or
//     checkpoints that name an agent the Captain no longer hosts,
//     surface as errdefs.NotFound — the agent topology has drifted
//     and a silent fallback to a different agent would be wrong.
//
// Resume reuses the same dispatch plumbing as Submit (admission
// gate, token budget, run ctx, handle), so observers / deciders
// fire the same way they did on the original attempt. The synthetic
// agent.Request carries only RunID == cp.ExecID; the engine's
// Resumer restores board state from cp.Board so the message body
// being empty is correct. Callers that want to inject metadata
// (a fresh ContextID, custom Inputs) can pass req via the
// lower-level [Submit] + [agent.WithResumeFrom] path on the
// engine factory side.
//
// Returns a [Handle] just like Submit; the caller can Wait on it
// for the resumed run's terminal state.
func (c *Captain) Resume(ctx context.Context, runID string) (*Handle, error) {
	if c.checkpointStore == nil {
		return nil, errdefs.NotAvailablef("vessel: resume requires a CheckpointStore (WithCheckpointStore not configured)")
	}
	if runID == "" {
		return nil, errdefs.Validationf("vessel: resume runID must be non-empty")
	}
	cp, err := c.checkpointStore.Load(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("vessel: load checkpoint %q: %w", runID, err)
	}
	if cp == nil {
		return nil, errdefs.NotFoundf("vessel: no checkpoint for runID %q", runID)
	}
	agentName := cp.Attributes[checkpointAttrAgentName]
	if agentName == "" {
		return nil, errdefs.NotFoundf(
			"vessel: checkpoint %q has no %s attribute; cannot route resume",
			runID, checkpointAttrAgentName)
	}
	if _, ok := c.entries[agentName]; !ok {
		return nil, errdefs.NotFoundf(
			"vessel: checkpoint %q targets agent %q, which is not hosted by this vessel",
			runID, agentName)
	}
	req := agent.Request{RunID: cp.ExecID}
	return c.submit(ctx, agentName, req, []agent.RunOption{agent.WithResumeFrom(cp)})
}

// submit is the shared dispatch path used by both Submit and Resume.
// extraOpts lets Resume inject agent.WithResumeFrom without
// duplicating the admission / handle / goroutine plumbing.
func (c *Captain) submit(ctx context.Context, agentName string, req agent.Request, extraOpts []agent.RunOption) (*Handle, error) {
	c.mu.Lock()
	phase := c.Phase()
	if !phase.AcceptsRequests() {
		c.mu.Unlock()
		return nil, errdefs.NotAvailablef("vessel: not accepting requests in phase %q", phase)
	}
	entry, ok := c.entries[agentName]
	if !ok {
		c.mu.Unlock()
		return nil, errdefs.NotFoundf("vessel: no agent named %q", agentName)
	}
	if entry.spec.Sidecar {
		c.mu.Unlock()
		return nil, errdefs.Conflictf("vessel: agent %q is a sidecar; trigger via bus, not Submit", agentName)
	}

	if c.budget.hourExhausted() {
		c.mu.Unlock()
		return nil, errdefs.RateLimitf("vessel: hourly token budget exhausted")
	}

	if req.RunID == "" {
		req.RunID = mintRunID()
	}
	h := newHandle(req.RunID, agentName)
	rootCtx := c.rootCtx
	// Keep Add under the lifecycle lock. Drain/Stop transition out of
	// PhaseRunning while holding the same lock and only call Wait after
	// releasing it, so no new Submit can race Add against Wait.
	c.inflight.Add(1)
	c.mu.Unlock()

	// runCtx merges caller ctx with the Captain's root so Stop
	// cancels in-flight runs even when the caller never cancels
	// its own ctx.
	parent, cancelParent := mergeContexts(ctx, rootCtx)

	go func() {
		defer c.inflight.Done()
		defer cancelParent()

		// Acquire the admission slot inside the goroutine so
		// Submit returns the Handle synchronously: callers can
		// observe waiting-for-slot via Handle.Wait + ctx, not by
		// having Submit itself block.
		runCtx, release, err := c.gate.acquire(parent)
		if err != nil {
			h.deliver(nil, err)
			return
		}
		defer release()

		// Stash a per-Run usage tracker on runCtx so the sandbox
		// host's ReportUsage can debit against it. release is the
		// runCtx cancel; budget.add invokes it when the per-turn
		// cap is breached, ending the engine's next iteration.
		ru := c.budget.begin(req.RunID, release)
		if ru != nil {
			runCtx = context.WithValue(runCtx, budgetCtxKey{}, ru)
			defer c.budget.end(req.RunID)
		}

		// Provision the per-run workspace BEFORE dispatch so engines
		// / tools see it via WorkspaceFromContext from the very first
		// node. Close runs on baseCtx (not runCtx) so cleanup
		// survives a runCtx cancellation — the Open path is what
		// reserved the resource, the Close path must always run.
		if c.sessionStore != nil {
			ws, err := c.sessionStore.Open(runCtx, req.RunID)
			if err != nil {
				h.deliver(nil, errdefs.Internalf("vessel: open session for run %q: %v", req.RunID, err))
				return
			}
			runCtx = context.WithValue(runCtx, sessionCtxKey{}, ws)
			defer func() {
				_ = c.sessionStore.Close(c.baseCtx, req.RunID)
			}()
		}

		res, runErr := c.dispatch(runCtx, entry, req, extraOpts...)
		if runErr != nil {
			telemetry.Warn(runCtx, "vessel: agent.Run returned error",
				otellog.String("vessel_id", c.vs.ID),
				otellog.String("agent_name", agentName),
				otellog.String("run_id", req.RunID),
				otellog.String("error", runErr.Error()))
		}
		h.deliver(res, runErr)
	}()

	return h, nil
}

// Call is the synchronous sugar over Submit + Handle.Wait. It
// blocks until the run reaches a terminal state and returns the
// agent.Result + error pair agent.Run produced.
//
// Call honours ctx cancellation the same way Wait does: when ctx
// fires before the run finishes, Call returns ctx.Err() and the
// underlying run keeps going — the Captain still tracks it via the
// inflight WaitGroup so Drain / Stop can wait for it.
func (c *Captain) Call(ctx context.Context, agentName string, req agent.Request) (*agent.Result, error) {
	h, err := c.Submit(ctx, agentName, req)
	if err != nil {
		return nil, err
	}
	return h.Wait(ctx)
}

// Drain transitions the Captain from PhaseRunning into PhaseDraining
// and waits for in-flight runs (including sidecar loops) to
// complete. New Submit / Call requests are rejected with
// errdefs.NotAvailable from the moment Drain starts.
//
// Drain returns when every in-flight run has finished or when ctx
// fires — whichever comes first. On ctx expiry, in-flight runs
// continue executing (Drain is the cooperative variant); call Stop
// for impatient teardown.
//
// After Drain returns successfully the phase advances to
// PhaseStopped and the bus (if owned) is closed.
func (c *Captain) Drain(ctx context.Context) error {
	c.mu.Lock()
	switch c.Phase() {
	case PhasePending:
		c.transitionPhaseLocked(PhaseStopped, "drain on pending")
		c.closeOwnedBus()
		c.mu.Unlock()
		return nil
	case PhaseRunning:
		c.transitionPhaseLocked(PhaseDraining, "")
	case PhaseDraining, PhaseStopping:
		// Already in teardown.
	case PhaseStopped, PhaseFailed:
		c.mu.Unlock()
		return nil
	default:
		c.mu.Unlock()
		return errdefs.Conflictf("vessel: cannot Drain from phase %q", c.Phase())
	}
	// Sidecars stop accepting new envelopes by closing their
	// subscriptions; in-flight sidecar dispatches are waited on
	// by the inflight WG below.
	c.stopSidecars()
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		c.finalize("drain complete")
		return nil
	case <-ctx.Done():
		// Drain expired; leave the phase at PhaseDraining so the
		// caller can either retry Drain with a fresh ctx or fall
		// back to Stop. Vessel resources are NOT released yet.
		return ctx.Err()
	}
}

// Stop is the impatient counterpart to Drain. It transitions to
// PhaseStopping, cancels the Captain's root context (which
// propagates ctx cancellation to every in-flight agent.Run and
// every sidecar loop), waits for the goroutines to exit, and
// finalises resources.
//
// Stop is bounded by ctx: when ctx fires the wait is abandoned but
// the cancellation has already been broadcast, so leaked goroutines
// will exit shortly after on their own (assuming the agent honours
// ctx, which the engine.Engine contract requires).
func (c *Captain) Stop(ctx context.Context) error {
	c.mu.Lock()
	switch c.Phase() {
	case PhasePending:
		c.transitionPhaseLocked(PhaseStopped, "stop on pending")
		c.closeOwnedBus()
		c.mu.Unlock()
		return nil
	case PhaseRunning, PhaseDraining, PhaseFailed:
		c.transitionPhaseLocked(PhaseStopping, "")
	case PhaseStopping:
	case PhaseStopped:
		c.mu.Unlock()
		return nil
	}
	if c.rootCancel != nil {
		c.rootCancel()
	}
	c.gate.close()
	c.probes.stop()
	c.stopSidecars()
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		c.finalize("stop complete")
		return nil
	case <-ctx.Done():
		// Best-effort: run goroutines have been signalled; do not
		// wait for them. finalize so the bus closes and the phase
		// reflects the request — leaked agent goroutines will
		// exit independently.
		c.finalize("stop ctx expired")
		return ctx.Err()
	}
}

// transitionToFailed is the bridge the probe loop uses to surface
// a fatal verdict. It runs without holding c.mu — the probe loop
// must remain non-blocking — but funnels through the locked
// section so the phase transition is monotonic.
//
// When the spec configures [spec.RestartOnFailure] the
// Captain spawns a recovery goroutine that waits the configured
// backoff and re-runs Launch. The probe runner is NOT restarted
// alongside; recovery succeeds only when probes themselves recover
// (Launch wires a fresh probeRunner referencing the same Probes
// list).
//
// MaxRestarts caps the number of consecutive failure cycles. Each
// transitionToFailed counts as one cycle (regardless of whether
// the cycle came from probe flap or from an in-loop Launch error);
// the counter resets only after the captain has stayed in
// PhaseRunning for restartStableWindow without re-failing. Once
// exhausted the captain runs finalize() — moving to PhaseStopped
// and closing owned resources — instead of leaving a zombie
// PhaseFailed Captain that the operator might miss.
func (c *Captain) transitionToFailed(reason string) {
	c.mu.Lock()
	if c.Phase() != PhaseRunning {
		c.mu.Unlock()
		return
	}
	c.transitionPhaseLocked(PhaseFailed, reason)
	// Tear down the probe loop, sidecars, and rootCtx so the
	// Captain looks like a fresh PhasePending instance to the
	// restart attempt below.
	c.probes.stop()
	c.probes = nil
	c.stopSidecars()
	if c.rootCancel != nil {
		c.rootCancel()
		c.rootCtx, c.rootCancel = nil, nil
	}
	mode := c.vs.Restart.Mode
	maxRestarts := c.vs.Restart.MaxRestarts
	// Pre-decision: if this transition has already maxed the
	// captain-level counter, skip restartLoop entirely and run
	// finalize. Without this gate, probe-driven flap would spawn
	// an unbounded sequence of restartLoops because each new
	// goroutine's own attempt counter resets to 0 — exactly the
	// bug PR-09 (and its e2e companion) is fixing.
	exhausted := mode == spec.RestartOnFailure &&
		maxRestarts > 0 &&
		c.restartAttempts >= maxRestarts
	c.mu.Unlock()

	if exhausted {
		c.finalize("restart attempts exhausted: " + reason)
		return
	}
	if mode != spec.RestartOnFailure {
		return
	}
	go c.restartLoop(reason)
}

// restartLoop waits the configured backoff and re-runs Launch.
// Backoff doubles after each failed attempt, capped at BackoffMax.
//
// The MaxRestarts counter lives on Captain (c.restartAttempts) and
// is shared across every restartLoop spawn. This is essential
// because probe-driven failures invoke transitionToFailed -> new
// restartLoop per cycle; if the counter were local to this
// goroutine each cycle would start at 0 and the cap would never
// trip. The counter is forgiven by transitionPhaseLocked when the
// captain has stayed in PhaseRunning for restartStableWindow,
// matching the kubernetes CrashLoopBackOff "stable run" semantic.
func (c *Captain) restartLoop(initialReason string) {
	cfg := c.vs.Restart
	if cfg.BackoffInit <= 0 {
		cfg.BackoffInit = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = time.Minute
	}
	wait := cfg.BackoffInit
	reason := initialReason
	for {
		select {
		case <-c.baseCtx.Done():
			return
		case <-time.After(wait):
		}

		// Bail out if Stop / Drain raced us between attempts.
		c.mu.Lock()
		if c.Phase() != PhaseFailed {
			c.mu.Unlock()
			return
		}
		c.restartAttempts++
		attempts := c.restartAttempts
		// Reset the probe runner from the spec so re-Launch sees
		// the same configuration as the original New().
		if c.vs.Probes != nil {
			c.probes = newProbeRunner(c, *c.vs.Probes)
		}
		c.transitionPhaseLocked(PhasePending, "restart attempt "+strconv.Itoa(attempts))
		c.mu.Unlock()

		telemetry.Warn(c.baseCtx, "vessel: restart attempt",
			otellog.String("vessel_id", c.vs.ID),
			otellog.Int("attempt", attempts),
			otellog.String("reason", reason))

		if err := c.Launch(c.baseCtx); err == nil {
			// Launch succeeded — restartLoop's job is done. The
			// captain is back in PhaseRunning; if probes flap
			// again, transitionToFailed will spawn a fresh
			// restartLoop and the shared restartAttempts counter
			// keeps going (forgiven only after a full
			// restartStableWindow of healthy runtime).
			return
		} else {
			reason = err.Error()
			// Launch failed: it left the captain in PhasePending
			// (Launch's rollback path drops rootCtx but does not
			// touch the phase). The next iteration's
			// "Phase() == PhaseFailed?" guard would short-circuit
			// the loop and never reach the MaxRestarts check
			// below — so finalize would never run, even though
			// we are clearly in a stuck state. Re-stamp the
			// phase explicitly so subsequent attempts (and the
			// exhaust check) see the correct "still failed" view.
			c.mu.Lock()
			if c.Phase() == PhasePending {
				c.transitionPhaseLocked(PhaseFailed, "restart attempt "+strconv.Itoa(attempts)+" failed: "+reason)
			}
			c.mu.Unlock()
		}

		if cfg.MaxRestarts > 0 && attempts >= cfg.MaxRestarts {
			// Exhaust path: this is the only restart-loop exit
			// after which no further Launch will be attempted, so
			// it is the right place to release vessel resources.
			// finalize transitions to PhaseStopped — the terminal
			// state callers already check for via
			// Phase().IsTerminal().
			c.finalize("restart attempts exhausted: " + reason)
			return
		}
		wait *= 2
		if wait > cfg.BackoffMax {
			wait = cfg.BackoffMax
		}
	}
}

// finalize is the common tail of Drain / Stop. It must only be
// called once per terminal transition; subsequent calls are no-ops
// because the phase guard short-circuits.
func (c *Captain) finalize(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Phase() == PhaseStopped {
		return
	}
	c.transitionPhaseLocked(PhaseStopped, reason)
	c.probes.stop()
	c.stopKanban()
	c.closeOwnedBus()
}

func (c *Captain) closeOwnedBus() {
	if c.busOwned && c.bus != nil {
		_ = c.bus.Close()
	}
}

// transitionPhaseLocked records the new phase and emits the bus
// envelope. Callers MUST hold c.mu.
//
// Two side-effects bake the captain-level restart counter into the
// state machine:
//
//   - Entering Running: stamp enteredRunningAt so the next
//     "leaving Running" event can decide whether to forgive the
//     accumulated restartAttempts.
//   - Leaving Running after restartStableWindow: reset
//     restartAttempts. Captures the "we ran healthy long enough
//     that this looks like a fresh failure, not a flap" semantic.
func (c *Captain) transitionPhaseLocked(next Phase, reason string) {
	prev := c.Phase()
	if prev == next {
		return
	}
	if prev == PhaseRunning && next != PhaseRunning {
		if !c.enteredRunningAt.IsZero() && time.Since(c.enteredRunningAt) >= stableWindow() {
			c.restartAttempts = 0
		}
	}
	if next == PhaseRunning {
		c.enteredRunningAt = time.Now()
	}
	c.phase.Store(next)
	if c.bus == nil {
		return
	}
	payload := PhaseChangedPayload{
		VesselID: c.vs.ID,
		From:     prev,
		To:       next,
		Reason:   reason,
	}
	env, err := event.NewEnvelope(c.baseCtx, SubjectPhaseChanged, payload)
	if err != nil {
		return
	}
	env.Source = c.vs.ID
	_ = c.bus.Publish(c.baseCtx, env)
}

// mintVesselID generates a "v-<hex>" identifier when the spec did
// not supply one. Mirrors agent.mintRunID's shape so vessel ids
// look uniform across telemetry.
func mintVesselID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "v-fallback"
	}
	return "v-" + hex.EncodeToString(b)
}

// mintRunID is the run-id minter the Captain uses when the caller
// did not supply one on agent.Request.
func mintRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "run-fallback"
	}
	return "run-" + hex.EncodeToString(b)
}

// mergeContexts returns a context that is cancelled whenever EITHER
// parent fires. Required because Submit must honour both the
// caller's ctx (per-request cancellation) and the Captain's root
// ctx (Stop broadcast). The returned cancel func MUST be called by
// the consumer to release the bookkeeping goroutine.
func mergeContexts(a, b context.Context) (context.Context, context.CancelFunc) {
	if a == nil {
		a = context.Background()
	}
	if b == nil {
		return context.WithCancel(a)
	}
	merged, cancel := context.WithCancel(a)
	stop := context.AfterFunc(b, cancel)
	return merged, func() {
		stop()
		cancel()
	}
}
