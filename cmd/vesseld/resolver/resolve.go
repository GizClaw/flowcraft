package resolver

import (
	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// Resolve translates a list of apispec Objects (typically the
// loader's output) into a Plan. When the configuration has any
// validation, reference, or factory error, Resolve returns the
// best-effort Plan plus a non-nil Errors aggregate; the fleet must
// not start when Errors is non-empty.
//
// The catalog argument is the source of factory implementations.
// Callers usually pass catalog.Builtin(); tests can pass a fresh
// New() with hand-registered factories.
//
// opts controls IO: validate-only callers set AllowFile/AllowSecret
// to false so file IO and live LLM client construction are skipped.
func Resolve(objs []apispec.Object, cat *catalog.Catalog, opts ResolveOptions) (*Plan, *Errors) {
	errs := &Errors{}

	inv, invErrs := buildInventory(objs)
	errs.addAll(invErrs)

	// Even when buildInventory produced errors we keep going with
	// the partial inventory: most errors are duplicates that do
	// not block the resolution of the other documents, and
	// surfacing every dependent error helps the user fix the file
	// in one pass.

	secretLookup, err := newSecretIndex(inv.Secrets)
	if err != nil {
		errs.add(err)
	}

	// Tool registry is daemon-shared; v0.1.0 leaves it empty
	// because no ToolPack factories ship in builtin. We allocate
	// it here so the Plan can carry the pointer to every Vessel.
	sharedToolReg := tool.NewRegistry()
	for _, tp := range inv.ToolPacks {
		fn, err := cat.ToolPack(tp.Spec.Ref)
		if err != nil {
			errs.add(err)
			continue
		}
		tools, err := fn(tp.Spec.Ref, tp.Spec.Config, catalog.Deps{ToolRegistry: sharedToolReg})
		if err != nil {
			errs.add(err)
			continue
		}
		for _, t := range tools {
			sharedToolReg.Register(t)
		}
	}

	llmClients, llmResolver, llmErrs := resolveLLMs(cat, inv.LLMProfiles, secretLookup, opts)
	errs.addAll(llmErrs)

	histories, histErrs := resolveHistories(cat, inv.HistoryStores, sharedToolReg, llmClients)
	errs.addAll(histErrs)

	sandboxes, sbErrs := resolveSandboxes(inv.Sandboxes)
	errs.addAll(sbErrs)

	plan := &Plan{
		SharedToolRegistry: sharedToolReg,
		SharedLLMResolver:  llmResolver,
		SharedHistories:    histories,
		SharedSandboxes:    sandboxes,
	}

	if len(inv.Daemons) >= 1 {
		plan.Daemon = buildDaemonPlan(inv.Daemons[0])
		store, err := buildSessionStore(inv.Daemons[0].Spec.SessionStore)
		if err != nil {
			errs.add(err)
		}
		plan.SharedSessionStore = store
	}

	for _, v := range inv.Vessels {
		vp, vErrs := resolveVessel(v, inv, cat, sharedToolReg, llmClients, histories, opts)
		errs.addAll(vErrs)
		plan.Vessels = append(plan.Vessels, vp)
	}

	return plan, errs
}

// buildDaemonPlan flattens the daemon document into the runtime-
// friendly DaemonPlan. Defaults are applied here (rather than in
// apispec validation) so the apispec layer stays a pure shape
// check while the resolver owns "what does an empty field mean".
func buildDaemonPlan(d v1alpha1.Daemon) DaemonPlan {
	dp := DaemonPlan{
		Name:              d.Name,
		Socket:            d.Spec.Control.Socket,
		Listen:            d.Spec.Control.Listen,
		TokenFile:         d.Spec.Control.Auth.TokenFile,
		MaxConcurrentRuns: d.Spec.Resources.MaxConcurrentRuns,
		LLMRateLimits:     d.Spec.LLMRateLimits,
		LoggingFormat:     d.Spec.Logging.Format,
		LoggingLevel:      d.Spec.Logging.Level,
		DrainTimeout:      d.Spec.Shutdown.DrainTimeout,
	}
	if dp.Socket == "" && dp.Listen == "" {
		dp.Socket = "/var/run/vesseld.sock"
	}
	if dp.LoggingFormat == "" {
		dp.LoggingFormat = "json"
	}
	if dp.LoggingLevel == "" {
		dp.LoggingLevel = "info"
	}
	if mtls := d.Spec.Control.Auth.MTLS; mtls != nil {
		minVer := mtls.MinVersion
		if minVer == "" {
			minVer = "1.3"
		}
		dp.MTLS = &DaemonMTLSPlan{
			CertRef:     mtls.Cert,
			KeyRef:      mtls.Key,
			ClientCARef: mtls.ClientCA,
			MinVersion:  minVer,
		}
	}
	return dp
}

// buildSessionStore materialises the daemon-wide vessel.SessionStore
// from the apispec block. Returns (nil, nil) when the operator did
// not configure sessionStore; the fleet treats that as "skip
// WithSessionStore" and tools fall back to their own wiring.
//
// FilesystemSessionStore's constructor calls os.MkdirAll on Root,
// so this is the first filesystem-touching step in the resolver
// path. That is consistent with the existing resolveLLMs / loader
// behaviour (resolver IS allowed to touch the filesystem; only
// apispec.Validate is required to stay side-effect free).
func buildSessionStore(ss *v1alpha1.DaemonSessionStore) (vessel.SessionStore, error) {
	if ss == nil {
		return nil, nil
	}
	switch ss.Backend {
	case "memory":
		return vessel.NewMemorySessionStore(), nil
	case "filesystem":
		store, err := vessel.NewFilesystemSessionStore(ss.Root)
		if err != nil {
			return nil, errdefs.Validationf("vesseld resolver: build filesystem session store at %q: %v", ss.Root, err)
		}
		return store, nil
	default:
		// apispec.Validate already covers this, but be defensive
		// so a misconfigured catch-around-Validate caller still
		// gets a structured error rather than a nil-store panic.
		return nil, errdefs.Validationf("vesseld resolver: unknown sessionStore backend %q", ss.Backend)
	}
}

// resolveHistories builds one history.History per HistoryStore
// document. Returns the name → instance map plus an Errors
// aggregate for any factory failures.
func resolveHistories(
	cat *catalog.Catalog,
	stores map[string]v1alpha1.HistoryStore,
	toolReg *tool.Registry,
	clients map[string]llm.LLM,
) (map[string]history.History, *Errors) {
	errs := &Errors{}
	out := make(map[string]history.History, len(stores))
	for name, hs := range stores {
		fn, err := cat.History(hs.Spec.Ref)
		if err != nil {
			errs.add(err)
			continue
		}
		inst, err := fn(hs.Spec.Ref, hs.Spec.Config, catalog.Deps{
			VesselID:     "",
			ToolRegistry: toolReg,
			LLMClients:   clients,
		})
		if err != nil {
			errs.add(err)
			continue
		}
		out[name] = inst
	}
	return out, errs
}

// resolveVessel walks one Vessel + its referenced Agents / Probes
// and produces a VesselPlan. The engine factory closure for each
// agent is resolved here so the fleet can hand it to vessel.New
// without touching the catalog itself.
func resolveVessel(
	v v1alpha1.Vessel,
	inv Inventory,
	cat *catalog.Catalog,
	toolReg *tool.Registry,
	clients map[string]llm.LLM,
	histories map[string]history.History,
	opts ResolveOptions,
) (VesselPlan, *Errors) {
	errs := &Errors{}
	vp := VesselPlan{
		Name:                   v.Name,
		EngineFactoriesByAgent: map[string]EngineBuilder{},
		EngineRefByAgent:       map[string]string{},
		Probes:                 map[string]spec.Probe{},
		SidecarSubscribeBy:     map[string]string{},
	}

	vs := spec.Spec{
		ID: v.Name,
	}

	// Agents.
	for _, agentRef := range v.Spec.Agents {
		agent, ok := inv.Agents[agentRef]
		if !ok {
			errs.add(errdefs.NotFoundf("vesseld Vessel %q: agent %q not found in loaded Agent docs", v.Name, agentRef))
			continue
		}
		specAgent, builder, aErrs := resolveAgent(v, agent, cat, toolReg, clients, histories[refOf(v.Spec.History)])
		errs.addAll(aErrs)
		if builder != nil {
			vp.EngineFactoriesByAgent[agent.Name] = builder
			vp.EngineRefByAgent[agent.Name] = agent.Spec.Engine.Ref
		}
		if specAgent.Sidecar {
			vp.SidecarSubscribeBy[agent.Name] = agent.Spec.SubscribeTo
		}
		if specAgent.Dispatcher {
			vp.DispatcherAgents = append(vp.DispatcherAgents, agent.Name)
		}
		// Sandbox reference handling. The agent's spec.sandbox
		// must name a known Sandbox document; v0.2.0 also
		// enforces "at most one Sandbox per Vessel" so the
		// per-Captain tool registry can register the auto-
		// generated `exec` tool under its canonical name
		// without disambiguation.
		if ref := agent.Spec.Sandbox; ref != "" {
			if _, ok := inv.Sandboxes[ref]; !ok {
				errs.add(errdefs.NotFoundf("vesseld Agent %q: spec.sandbox %q not found in loaded Sandbox docs", agent.Name, ref))
			} else {
				if vp.SandboxName == "" {
					vp.SandboxName = ref
				} else if vp.SandboxName != ref {
					errs.add(errdefs.Validationf(
						"vesseld Vessel %q: agents reference %d distinct Sandboxes (%q and %q); v0.2.0 supports at most one Sandbox per Vessel",
						v.Name, 2, vp.SandboxName, ref))
				}
				vp.SandboxAgents = append(vp.SandboxAgents, agent.Name)
			}
		}
		vs.Agents = append(vs.Agents, specAgent)
	}

	// History reference.
	if v.Spec.History != nil {
		if _, ok := histories[v.Spec.History.Ref]; !ok {
			errs.add(errdefs.NotFoundf("vesseld Vessel %q: spec.history.ref %q not found in loaded HistoryStore docs", v.Name, v.Spec.History.Ref))
		}
		vp.HistoryName = v.Spec.History.Ref
	}

	// Resources / restart / kanban.
	vs.Resources = spec.Resources{
		MaxConcurrentRuns: v.Spec.Resources.MaxConcurrentRuns,
		TurnTimeout:       v.Spec.Resources.TurnTimeout,
		MaxTokensPerTurn:  v.Spec.Resources.MaxTokensPerTurn,
		MaxTokensPerHour:  v.Spec.Resources.MaxTokensPerHour,
	}
	switch v.Spec.Restart.Mode {
	case "", "never":
		vs.Restart = spec.Restart{Mode: spec.RestartNever}
	case "on_failure":
		vs.Restart = spec.Restart{
			Mode:        spec.RestartOnFailure,
			MaxRestarts: v.Spec.Restart.MaxRestarts,
			BackoffInit: v.Spec.Restart.BackoffInit,
			BackoffMax:  v.Spec.Restart.BackoffMax,
		}
	}
	if v.Spec.Kanban != nil {
		vs.Kanban = &spec.Kanban{
			MaxPendingTasks:    v.Spec.Kanban.MaxPendingTasks,
			MaxProducerChain:   v.Spec.Kanban.MaxProducerChain,
			CallbackMaxSummary: v.Spec.Kanban.CallbackMaxSummary,
		}
	}

	// Probes.
	// Validate-only mode (no IO) cannot materialise probes that
	// depend on live LLM clients; skip the construction loop and
	// only verify the refs themselves resolve.
	validateOnly := !opts.AllowFile && !opts.AllowSecret
	if v.Spec.Probes != nil {
		probes := spec.Probes{
			Interval:         v.Spec.Probes.Interval,
			Timeout:          v.Spec.Probes.Timeout,
			FailureThreshold: v.Spec.Probes.FailureThreshold,
		}
		for _, ref := range v.Spec.Probes.Liveness {
			probeDoc, ok := inv.Probes[ref]
			if !ok {
				errs.add(errdefs.NotFoundf("vesseld Vessel %q: spec.probes.liveness ref %q not found", v.Name, ref))
				continue
			}
			fn, err := cat.Probe(probeDoc.Spec.Ref)
			if err != nil {
				errs.add(err)
				continue
			}
			if validateOnly {
				// Skip live construction; the catalog ref check
				// above is enough to flag typos at validate time.
				continue
			}
			inst, err := fn(probeDoc.Spec.Ref, probeDoc.Spec.Config, catalog.Deps{
				VesselID:     v.Name,
				LLMClients:   clients,
				ToolRegistry: toolReg,
			})
			if err != nil {
				errs.add(err)
				continue
			}
			vp.Probes[ref] = inst
			probes.Liveness = append(probes.Liveness, inst)
		}
		vs.Probes = &probes
	}

	vp.Spec = vs
	return vp, errs
}

// resolveAgent translates one v1alpha1.Agent into a spec.Agent
// plus a closure that constructs the engine.Engine on demand.
// Engine construction is deferred (closure) because the fleet
// passes per-Captain Bus / Host references that the resolver does
// not have at Plan time.
func resolveAgent(
	v v1alpha1.Vessel,
	a v1alpha1.Agent,
	cat *catalog.Catalog,
	toolReg *tool.Registry,
	clients map[string]llm.LLM,
	hist history.History,
) (spec.Agent, EngineBuilder, *Errors) {
	errs := &Errors{}
	specAgent := spec.Agent{
		Name:          a.Name,
		Tools:         append([]string(nil), a.Spec.Tools...),
		HistoryAccess: parseHistoryAccess(a.Spec.HistoryAccess, v.Spec.History != nil),
		Dispatcher:    a.Spec.Dispatcher,
		ProducerChain: a.Spec.ProducerChain,
		Sidecar:       a.Spec.Sidecar,
		SubscribeTo:   a.Spec.SubscribeTo,
	}
	// Card is `any` on spec.Agent; we always populate a
	// concrete agent.AgentCard so downstream consumers (A2A
	// discovery, dashboard) can type-assert without nil checks.
	card := agent.AgentCard{Name: a.Name}
	if a.Spec.Card != nil {
		if a.Spec.Card.Name != "" {
			card.Name = a.Spec.Card.Name
		}
		card.Description = a.Spec.Card.Description
	}
	specAgent.Card = card
	if specAgent.Dispatcher && v.Spec.Kanban == nil {
		errs.add(errdefs.Validationf("vesseld Agent %q: spec.dispatcher=true requires the parent Vessel %q to set spec.kanban", a.Name, v.Name))
	}

	// Resolve the engine factory closure now so we surface
	// missing-ref errors at validate time, not at first dispatch.
	fn, err := cat.Engine(a.Spec.Engine.Ref)
	if err != nil {
		errs.add(err)
		return specAgent, nil, errs
	}

	builder := EngineBuilder(func(rd RuntimeDeps) (engineBuildResult, error) {
		reg := toolReg
		if rd.ToolRegistry != nil {
			reg = rd.ToolRegistry
		}
		eng, err := fn(a.Spec.Engine.Ref, a.Spec.Engine.Config, catalog.Deps{
			VesselID:     v.Name,
			AgentName:    a.Name,
			AgentTools:   rd.AgentTools,
			Bus:          nil, // fleet injects per-Captain bus before invoking
			History:      hist,
			ToolRegistry: reg,
			LLMClients:   clients,
			LLMLimiters:  rd.LLMLimiters,
		})
		if err != nil {
			return engineBuildResult{}, err
		}
		// Type-assert the engine.Engine concretely so callers get
		// a clear panic rather than a silent type mismatch when a
		// factory returns the wrong shape.
		if _, ok := eng.(engine.Engine); !ok {
			return engineBuildResult{}, errdefs.Validationf("vesseld engine factory %q returned non-engine.Engine value (%T)", a.Spec.Engine.Ref, eng)
		}
		return engineBuildResult{Engine: eng}, nil
	})
	return specAgent, builder, errs
}

// parseHistoryAccess maps the wire string to the vesselspec enum.
// The "" default depends on whether the vessel has history configured
// at all: with history, default to read_write; without, default
// to none.
func parseHistoryAccess(s string, hasHistory bool) spec.HistoryAccess {
	switch s {
	case "none":
		return spec.HistoryAccessNone
	case "read_only":
		return spec.HistoryAccessReadOnly
	case "read_write":
		return spec.HistoryAccessReadWrite
	default:
		if hasHistory {
			return spec.HistoryAccessReadWrite
		}
		return spec.HistoryAccessNone
	}
}

// refOf returns the .Ref of a NamedRef pointer, or "" when nil.
func refOf(r *v1alpha1.NamedRef) string {
	if r == nil {
		return ""
	}
	return r.Ref
}

// _ ensures we keep the event import compiled-in: the resolver
// itself does not publish events but downstream packages
// (cli/plan, fleet) do, and importing here keeps the module graph
// flat.
var _ = event.Bus(nil)
