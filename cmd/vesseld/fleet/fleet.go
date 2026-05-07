package fleet

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// Fleet owns every Captain in the daemon process plus the
// daemon-wide concurrency gate and per-LLMProfile rate limiters.
//
// One instance is constructed per daemon run from a resolver.Plan
// via Build. The Fleet is goroutine-safe; the HTTP layer holds a
// long-lived reference and routes incoming requests through Submit
// / Drain / Stop.
type Fleet struct {
	plan resolver.Plan

	mu       sync.RWMutex
	captains map[string]*captainEntry

	// gate caps daemon-wide concurrent Submits when the daemon
	// document set spec.resources.maxConcurrentRuns. Nil when
	// the cap is 0 (unbounded).
	gate chan struct{}

	// limiters holds per-LLMProfile rate limiters. Built once at
	// construction; engine factories pull the right one out via
	// LimiterFromContext after Submit injects it.
	limiters map[string]*tokenBucket

	// runs caches terminal state for every Submit so HTTP callers
	// can poll /v1/runs/{run_id} after a fire-and-forget submit.
	// Without it the only path to observe completion was SSE logs,
	// which carry stream deltas but not the final agent.Result.
	runs *runRegistry

	// runSweeperStop signals the GC goroutine to exit on Stop.
	runSweeperStop chan struct{}
	runSweeperDone chan struct{}
}

// captainEntry pairs a live Captain with the per-vessel bus we
// constructed for it (so subscribers like log streaming can find
// it by vessel name without going through the Captain).
type captainEntry struct {
	cap *vessel.Captain
	bus event.Bus
}

// Build constructs every Captain declared in the plan, in plan
// order. Each Captain gets its own event.Bus so log subscribers
// can scope by vessel; the daemon-shared tool registry / LLM
// clients / history stores are passed through unchanged so they
// remain shared across all vessels.
//
// If any Captain fails to construct, Build stops and tears down
// the Captains it already created — partial fleets are never
// returned.
func Build(plan resolver.Plan) (*Fleet, error) {
	f := &Fleet{
		plan:           plan,
		captains:       make(map[string]*captainEntry, len(plan.Vessels)),
		limiters:       buildLimiters(plan.Daemon.LLMRateLimits),
		runs:           newRunRegistry(time.Hour),
		runSweeperStop: make(chan struct{}),
		runSweeperDone: make(chan struct{}),
	}
	if plan.Daemon.MaxConcurrentRuns > 0 {
		f.gate = make(chan struct{}, plan.Daemon.MaxConcurrentRuns)
	}
	go f.runSweeperLoop()

	for _, vp := range plan.Vessels {
		ent, err := f.buildCaptain(vp)
		if err != nil {
			f.teardownAll()
			return nil, err
		}
		f.captains[vp.Name] = ent
	}
	return f, nil
}

// Launch starts every Captain. Returns the first non-nil Launch
// error after attempting all of them — partial launches are kept
// running so debugging the failed one is easy via the API surface.
func (f *Fleet) Launch(ctx context.Context) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var firstErr error
	for _, ent := range f.captains {
		if err := ent.cap.Launch(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Captain returns the captain entry for the named vessel or
// errdefs.NotFound. Returned pointer is owned by the fleet —
// callers must not call Stop on it directly.
func (f *Fleet) Captain(vesselName string) (*vessel.Captain, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	ent, ok := f.captains[vesselName]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld: vessel %q not found", vesselName)
	}
	return ent.cap, nil
}

// Bus returns the event.Bus dedicated to the named vessel. Used
// by api/SSE streaming to subscribe per-vessel.
func (f *Fleet) Bus(vesselName string) (event.Bus, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	ent, ok := f.captains[vesselName]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld: vessel %q not found", vesselName)
	}
	return ent.bus, nil
}

// Names returns the vessel names in plan order. Stable ordering
// matters for `vesseld plan` and the /v1/vessels listing.
func (f *Fleet) Names() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	names := make([]string, 0, len(f.captains))
	for _, vp := range f.plan.Vessels {
		if _, ok := f.captains[vp.Name]; ok {
			names = append(names, vp.Name)
		}
	}
	return names
}

// Submit dispatches an agent.Request through the named vessel's
// Captain after passing the daemon-wide concurrency gate and
// injecting the per-profile rate limiter into the context. Returns
// the vessel.Handle the caller can wait on.
func (f *Fleet) Submit(ctx context.Context, vesselName, agentName string, req agent.Request) (*vessel.Handle, error) {
	cap, err := f.Captain(vesselName)
	if err != nil {
		return nil, err
	}
	if err := f.acquireGate(ctx); err != nil {
		return nil, err
	}
	h, err := cap.Submit(ctx, agentName, req)
	if err != nil {
		f.releaseGate()
		return nil, err
	}
	// Track the run so HTTP /v1/runs/{run_id} can answer after
	// the Submit caller has long since disconnected. Cheap when
	// nobody queries (registry is just a map write); essential
	// for the fire-and-forget submit endpoint. track() registers
	// its own OnTerminate hook to record terminal state.
	f.runs.track(vesselName, h)
	// Release the daemon-wide concurrency slot when the run
	// terminates. OnTerminate fires synchronously inside the
	// captain's dispatch goroutine BEFORE Done is closed, so
	// the slot is freed in time for any waiting Submit to claim
	// it without an extra goroutine round-trip.
	h.OnTerminate(func(_ *agent.Result, _ error) {
		f.releaseGate()
	})
	return h, nil
}

// LookupRun returns the registry entry for runID. Used by the
// /v1/runs/{run_id} HTTP endpoint to surface terminal status,
// messages and error after the original Submit caller has gone.
func (f *Fleet) LookupRun(runID string) (RunStatus, error) {
	e, err := f.runs.lookup(runID)
	if err != nil {
		return RunStatus{}, err
	}
	state := "running"
	if !e.CompletedAt.IsZero() {
		state = "completed"
		if e.Err != nil {
			state = "error"
		} else if e.Status != "" {
			state = string(e.Status)
		}
	}
	out := RunStatus{
		RunID:      e.RunID,
		VesselName: e.VesselName,
		AgentName:  e.AgentName,
		State:      state,
		StartedAt:  e.StartedAt,
		Messages:   e.Messages,
	}
	if !e.CompletedAt.IsZero() {
		out.CompletedAt = &e.CompletedAt
	}
	if e.Err != nil {
		out.Error = e.Err.Error()
	}
	return out, nil
}

// ListRuns returns up to opts.PageSize runs in newest-first order,
// optionally filtered by vessel and terminal state. The returned
// NextPageToken is opaque to callers and round-trips through the
// HTTP layer; an empty token signals "end of stream".
//
// In-memory only: see runRegistry doc for retention semantics.
// Operators that need durable history should consume bus envelopes.
func (f *Fleet) ListRuns(opts ListRunsOptions) ListRunsPage {
	pageSize := opts.PageSize
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 50
	}
	startedBefore, runIDAfter := decodePageToken(opts.PageToken)

	// We deliberately ask the registry for one extra entry so we
	// can detect "is there another page?" without a second pass.
	entries := f.runs.list(runListOptions{
		vessel:        opts.Vessel,
		state:         opts.State,
		pageSize:      pageSize + 1,
		startedBefore: startedBefore,
		runIDAfter:    runIDAfter,
	})

	out := ListRunsPage{Runs: make([]RunStatus, 0, pageSize)}
	for i, e := range entries {
		if i == pageSize {
			out.NextPageToken = encodePageToken(entries[i-1].StartedAt, entries[i-1].RunID)
			break
		}
		out.Runs = append(out.Runs, runEntryToStatus(e))
	}
	return out
}

// ListRunsOptions controls Fleet.ListRuns. Vessel / State are
// optional; an empty string disables the corresponding filter.
type ListRunsOptions struct {
	Vessel    string
	State     string
	PageSize  int
	PageToken string
}

// ListRunsPage is the HTTP-friendly projection of a paged result.
type ListRunsPage struct {
	Runs          []RunStatus `json:"runs"`
	NextPageToken string      `json:"next_page_token,omitempty"`
}

func base64URL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func base64URLDecode(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return string(b), err
}

// runEntryToStatus mirrors LookupRun's state derivation so the
// list and lookup endpoints report identical State strings.
func runEntryToStatus(e runEntry) RunStatus {
	state := "running"
	if !e.CompletedAt.IsZero() {
		state = "completed"
		if e.Err != nil {
			state = "error"
		} else if e.Status != "" {
			state = string(e.Status)
		}
	}
	out := RunStatus{
		RunID:      e.RunID,
		VesselName: e.VesselName,
		AgentName:  e.AgentName,
		State:      state,
		StartedAt:  e.StartedAt,
		Messages:   e.Messages,
	}
	if !e.CompletedAt.IsZero() {
		ct := e.CompletedAt
		out.CompletedAt = &ct
	}
	if e.Err != nil {
		out.Error = e.Err.Error()
	}
	return out
}

// encodePageToken renders the keyset cursor as URL-safe base64 so
// callers don't accidentally rely on internal token shape.
func encodePageToken(startedBefore time.Time, runID string) string {
	return base64URL(fmt.Sprintf("%d|%s", startedBefore.UnixNano(), runID))
}

// decodePageToken is forgiving — a malformed token is silently
// treated as "no cursor", returning the first page.
func decodePageToken(tok string) (time.Time, string) {
	if tok == "" {
		return time.Time{}, ""
	}
	raw, err := base64URLDecode(tok)
	if err != nil {
		return time.Time{}, ""
	}
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		return time.Time{}, ""
	}
	ns, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, ""
	}
	return time.Unix(0, ns), parts[1]
}

// RunStatus is the public, JSON-friendly projection of one tracked
// run. Returned by Fleet.LookupRun and rendered by the HTTP layer.
type RunStatus struct {
	RunID       string          `json:"run_id"`
	VesselName  string          `json:"vessel"`
	AgentName   string          `json:"agent"`
	State       string          `json:"state"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Messages    []model.Message `json:"messages,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// runSweeperLoop GC's old registry entries every minute. Exits on
// Stop via runSweeperStop; closing runSweeperDone signals exit.
func (f *Fleet) runSweeperLoop() {
	defer close(f.runSweeperDone)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-f.runSweeperStop:
			return
		case now := <-t.C:
			f.runs.sweep(now)
		}
	}
}

// Drain calls Drain on every Captain concurrently and returns
// after all of them complete (or the context expires). Used by
// runtime/SIGTERM handler.
func (f *Fleet) Drain(ctx context.Context) error {
	f.mu.RLock()
	captains := make([]*captainEntry, 0, len(f.captains))
	for _, ent := range f.captains {
		captains = append(captains, ent)
	}
	f.mu.RUnlock()

	errCh := make(chan error, len(captains))
	for _, ent := range captains {
		ent := ent
		go func() {
			errCh <- ent.cap.Drain(ctx)
		}()
	}
	var firstErr error
	for range captains {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Stop calls Stop on every Captain concurrently. Used after
// Drain returns or after the drain timeout fires.
func (f *Fleet) Stop(ctx context.Context) error {
	f.mu.Lock()
	captains := make([]*captainEntry, 0, len(f.captains))
	for _, ent := range f.captains {
		captains = append(captains, ent)
	}
	// Clear the map upfront so concurrent Submits get NotFound
	// rather than racing the in-flight stops.
	f.captains = map[string]*captainEntry{}
	f.mu.Unlock()

	// Stop the run-registry sweeper. Idempotent: closing a closed
	// channel would panic, so guard with a select on the done
	// signal — Stop may be called twice in test paths.
	select {
	case <-f.runSweeperDone:
	default:
		close(f.runSweeperStop)
		<-f.runSweeperDone
	}

	errCh := make(chan error, len(captains))
	for _, ent := range captains {
		ent := ent
		go func() {
			errCh <- ent.cap.Stop(ctx)
		}()
	}
	var firstErr error
	for range captains {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// acquireGate / releaseGate implement the daemon-wide concurrency
// cap. When gate is nil (unbounded) both are no-ops.
func (f *Fleet) acquireGate(ctx context.Context) error {
	if f.gate == nil {
		return nil
	}
	select {
	case f.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fleet) releaseGate() {
	if f.gate == nil {
		return
	}
	select {
	case <-f.gate:
	default:
	}
}

// teardownAll is the rollback used when Build fails partway
// through. Each successful Captain is Stop()-ed with a short
// timeout to avoid blocking startup forever on a misconfigured
// vessel.
func (f *Fleet) teardownAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ent := range f.captains {
		_ = ent.cap.Stop(ctx)
	}
	f.captains = map[string]*captainEntry{}
}

// buildCaptain wires one VesselPlan into a live Captain. The
// engine factory closure stored in the VesselPlan is wrapped so
// the Captain's per-agent invocation receives the per-vessel bus
// + history + tool registry through vessel.Deps.
func (f *Fleet) buildCaptain(vp resolver.VesselPlan) (*captainEntry, error) {
	bus := event.NewMemoryBus()
	options := []vessel.Option{
		vessel.WithBus(bus),
		vessel.WithToolRegistry(f.plan.SharedToolRegistry),
		vessel.WithLLMResolver(f.plan.SharedLLMResolver),
		vessel.WithEngineFactory(func(aspec spec.Agent, deps vessel.Deps) (engine.Engine, error) {
			builder, ok := vp.EngineFactoriesByAgent[aspec.Name]
			if !ok {
				return nil, errdefs.NotFoundf("vesseld fleet: no engine builder for %s/%s", vp.Name, aspec.Name)
			}
			// aspec.Tools at this layer already carries the
			// vessel-runtime kanban auto-injection for Dispatcher
			// agents (vessel/router.go augmentedAgentTools); pass
			// it straight through so the engine factory honours
			// the same allow-list the agent.Agent will be built
			// with.
			res, err := builder(resolver.RuntimeDeps{
				AgentTools:  aspec.Tools,
				LLMLimiters: f.limitersAsCatalog(),
			})
			if err != nil {
				return nil, err
			}
			eng, ok := res.Engine.(engine.Engine)
			if !ok {
				return nil, errdefs.Validationf("vesseld fleet: engine builder for %s/%s returned %T, not engine.Engine", vp.Name, aspec.Name, res.Engine)
			}
			return eng, nil
		}),
	}
	if vp.HistoryName != "" {
		if h, ok := f.plan.SharedHistories[vp.HistoryName]; ok {
			options = append(options, vessel.WithHistory(h))
		}
	}

	cap, err := vessel.New(vp.Spec, options...)
	if err != nil {
		return nil, err
	}
	return &captainEntry{cap: cap, bus: bus}, nil
}

// Sorted returns a sorted copy of the vessel-name list. Helpful
// for /v1/vessels JSON payloads that callers may diff in CI.
func (f *Fleet) Sorted() []string {
	out := f.Names()
	sort.Strings(out)
	return out
}

// VesselPlan returns the resolved [resolver.VesselPlan] for the
// named vessel — useful for read-only API surfaces (e.g. the plan
// dump endpoint) that need to project static configuration without
// going through the live Captain. Returns ok=false when no vessel
// with that name was declared.
func (f *Fleet) VesselPlan(name string) (resolver.VesselPlan, bool) {
	for _, vp := range f.plan.Vessels {
		if vp.Name == name {
			return vp, true
		}
	}
	return resolver.VesselPlan{}, false
}

// DaemonPlan returns the resolved daemon-level plan. Used by the
// /plan endpoint to surface daemon name + drain timeout etc.
func (f *Fleet) DaemonPlan() resolver.DaemonPlan {
	return f.plan.Daemon
}

// limitersAsCatalog projects the fleet's per-LLMProfile token
// buckets onto the [catalog.Limiter] interface so the resolver
// closure (which lives in the catalog package's import scope) can
// pass them straight into [catalog.Deps.LLMLimiters]. Returns nil
// when no limiters were configured — engine factories then treat
// the absence as "no limit".
func (f *Fleet) limitersAsCatalog() map[string]catalog.Limiter {
	if len(f.limiters) == 0 {
		return nil
	}
	out := make(map[string]catalog.Limiter, len(f.limiters))
	for name, lim := range f.limiters {
		out[name] = lim
	}
	return out
}
