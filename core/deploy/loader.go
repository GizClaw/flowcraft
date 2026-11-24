package deploy

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// Loader resolves documents against one application's registered kinds.
//
// It holds the implementation table and nothing else: no built object, no cached state. The same
// loader serves the process document and every agent document, which is what makes "an implementation
// this process does not carry" a single error message instead of two spellings.
type Loader struct {
	impls *Impls
	// configOptions are the placeholder resolvers the application registered. A document writes
	// `${scheme:args}` where a value cannot be written down, and only the binary knows which schemes it
	// can resolve — so they travel with the loader, not with a layer.
	configOptions []config.Option
}

// NewLoader returns a loader over the application's registration table.
func NewLoader(impls *Impls) *Loader { return &Loader{impls: impls} }

// WithConfigOptions returns a loader that expands the placeholders these resolvers answer.
//
// It returns a copy rather than mutating: a loader is a value an application may keep and refine per
// load, and a method that changed its receiver would make the second load of a workspace depend on what
// the first one registered. Only ${env:...} is built in; anything else is the application's to name.
func (l *Loader) WithConfigOptions(opts ...config.Option) *Loader {
	refined := *l
	refined.configOptions = append(append([]config.Option(nil), l.configOptions...), opts...)
	return &refined
}

// Process resolves the process document into a plan.
//
// Every entry's settings are decoded by its factory here, so the document is fully validated
// before anything is built. Nothing is dialled, started or opened.
func (l *Loader) Process(ctx context.Context, layers ...config.Layer) (ProcessPlan, error) {
	if err := l.ready(); err != nil {
		return ProcessPlan{}, err
	}
	doc, err := loadDocument[processDocument](ctx, layers, "runtime", l.configOptions)
	if err != nil {
		return ProcessPlan{}, err
	}

	plan := ProcessPlan{SkillRoots: append([]string(nil), doc.Skills.Roots...)}
	seen := map[string]struct{}{}

	// The record level is read first: the chain below is filtered by it, so a document whose
	// observability block is wrong reports that before it reports anything about a middleware.
	level, err := observability(doc.Observability)
	if err != nil {
		return ProcessPlan{}, err
	}
	plan.Observability = level

	for _, entry := range doc.Inference.Providers {
		planned, err := l.planProvider(entry)
		if err != nil {
			return ProcessPlan{}, err
		}
		if err := takeName(seen, "provider", planned.Name); err != nil {
			return ProcessPlan{}, err
		}
		plan.Providers = append(plan.Providers, planned)
	}
	for _, entry := range doc.Sandbox {
		planned, err := l.planBackend(entry)
		if err != nil {
			return ProcessPlan{}, err
		}
		if err := takeName(seen, "sandbox", planned.Name); err != nil {
			return ProcessPlan{}, err
		}
		plan.Backends = append(plan.Backends, planned)
	}
	for _, entry := range doc.Tools.Sources {
		planned, err := l.planToolSource(entry)
		if err != nil {
			return ProcessPlan{}, err
		}
		if err := takeName(seen, "tool source", planned.Name); err != nil {
			return ProcessPlan{}, err
		}
		plan.Tools = append(plan.Tools, planned)
	}
	for _, entry := range doc.Tools.Middleware {
		planned, err := l.planMiddleware(entry)
		if err != nil {
			return ProcessPlan{}, err
		}
		if err := takeName(seen, "middleware", planned.Name); err != nil {
			return ProcessPlan{}, err
		}
		// A link whose registration says it is only worth installing above this level is left
		// out here rather than built and disabled: the plan is what the chain will be.
		if planned.Trace > level.Trace {
			continue
		}
		plan.Middleware = append(plan.Middleware, planned)
	}

	route, err := parseRoute(doc.Inference.Route)
	if err != nil {
		return ProcessPlan{}, err
	}
	plan.Route = route
	return plan, nil
}

// Deployments resolves an agent document against what the process offers.
//
// One document carries a list of agents, because a fleet is described once and then tuned per
// seat — and because a document with a single agent is just a list of one. Layer merging uses
// the same list: an overlay that rewrites one agent's grant names that agent and leaves the
// rest alone.
//
// The split between the two error classes happens here: decoding is a document error, and
// everything checked against caps is [errdefs.NotAvailable] — the document is fine, this
// process cannot serve it. Every failure names the agent it is about, since the document no
// longer has one.
func (l *Loader) Deployments(ctx context.Context, caps Capabilities, layers ...config.Layer) ([]Deployment, error) {
	if err := l.ready(); err != nil {
		return nil, err
	}
	doc, err := loadDocument[agentDocument](ctx, layers, "agent", l.configOptions)
	if err != nil {
		return nil, err
	}
	if len(doc.Agents) == 0 {
		return nil, errdefs.Validationf("deploy: an agent document needs at least one agent")
	}

	seen := make(map[string]struct{}, len(doc.Agents))
	out := make([]Deployment, 0, len(doc.Agents))
	for _, entry := range doc.Agents {
		deployment, err := l.resolveAgent(entry, caps)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[deployment.Name]; duplicate {
			return nil, errdefs.Validationf(
				"deploy: agent %q is declared twice; a document names each agent once",
				deployment.Name)
		}
		seen[deployment.Name] = struct{}{}
		out = append(out, deployment)
	}
	return out, nil
}

// resolveAgent is one agent's resolution: its program, its authority, and the hook names the
// host must resolve. How much of a turn is recorded is not here: it is the process's, and the
// process document states it.
func (l *Loader) resolveAgent(entry agentEntry, caps Capabilities) (Deployment, error) {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		return Deployment{}, errdefs.Validationf("deploy: every agent needs a name")
	}
	if err := entry.Grant.Validate(); err != nil {
		return Deployment{}, errdefs.Validationf("deploy: agent %q: %w", name, err)
	}
	// The limits block is required: there is no inherited default, and a session table that
	// silently keeps every conversation is not something a document may forget into.
	if entry.Grant.Limits.IsZero() {
		return Deployment{}, errdefs.Validationf(
			"deploy: agent %q must state grant.limits (idle, and optionally max_sessions)", name)
	}
	if entry.Grant.Limits.Idle <= 0 {
		return Deployment{}, errdefs.Validationf(
			"deploy: agent %q: grant.limits.idle must be a positive duration", name)
	}

	program, err := l.planProgram(entry.Program)
	if err != nil {
		return Deployment{}, errdefs.Validationf("deploy: agent %q: %w", name, err)
	}
	if err := checkGrant(name, entry.Grant, caps); err != nil {
		return Deployment{}, err
	}
	hooks, err := l.planHooks(name, entry.Hooks)
	if err != nil {
		return Deployment{}, err
	}

	return Deployment{
		Name:    name,
		Program: program,
		Grant:   entry.Grant,
		Hooks:   hooks,
	}, nil
}

// planHooks resolves one agent's hook list.
//
// An implementation nobody registered is a document error rather than NotAvailable: the hook table is the
// application's build-time registration, exactly like the provider and program tables, and a
// document naming an implementation that does not exist is one that was not checked against the
// binary it ships with.
func (l *Loader) planHooks(agentName string, docs []hookDocument) ([]PlannedHook, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	hooks := make([]PlannedHook, 0, len(docs))
	for _, doc := range docs {
		planned, err := l.plan(GroupHook, fmt.Sprintf("agent %q hook", agentName), doc.Impl, doc.Settings)
		if err != nil {
			return nil, err
		}
		build := planned.make
		hooks = append(hooks, PlannedHook{
			Impl: planned.impl,
			New: func() (agent.Hooks, error) {
				built, err := build()
				if err != nil {
					return agent.Hooks{}, err
				}
				hooks, ok := built.(agent.Hooks)
				if !ok {
					return agent.Hooks{}, errdefs.Internalf(
						"deploy: hook kind %q built %T, want agent.Hooks", planned.impl, built)
				}
				return hooks, nil
			},
		})
	}
	return hooks, nil
}

// checkGrant is the document-versus-process check for authority: a grant that names a
// backend or a model this process does not carry is a deployment that would fail on its
// first session, and saying so at install time is the whole point of this layer.
func checkGrant(agentName string, grant policy.Grant, caps Capabilities) error {
	if sandboxName := strings.TrimSpace(grant.Sandbox); sandboxName != "" {
		if !slices.Contains(caps.Backends, sandboxName) {
			return errdefs.NotAvailablef(
				"deploy: agent %q selects sandbox %q, which this process does not configure (%s)",
				agentName, sandboxName, listOf(caps.Backends))
		}
	}
	for _, model := range grant.Models {
		ref := model.ID.Provider + "/" + model.ID.Name
		if !slices.Contains(caps.Models, ref) {
			return errdefs.NotAvailablef(
				"deploy: agent %q names model %q, which this process does not serve (%s)",
				agentName, ref, listOf(caps.Models))
		}
	}
	return nil
}

func (l *Loader) planProvider(entry entryDocument) (PlannedProvider, error) {
	planned, err := l.plan(GroupProvider, fmt.Sprintf("provider %q", entry.Name), entry.Impl, entry.Settings)
	if err != nil {
		return PlannedProvider{}, err
	}
	build := planned.make
	return PlannedProvider{
		Name: entry.Name,
		Impl: planned.impl,
		New: func() (*inference.Provider, error) {
			built, err := build()
			if err != nil {
				return nil, err
			}
			provider, ok := built.(*inference.Provider)
			if !ok {
				return nil, errdefs.Internalf(
					"deploy: provider kind %q built %T, want *inference.Provider", planned.impl, built)
			}
			return provider, nil
		},
	}, nil
}

func (l *Loader) planBackend(entry entryDocument) (PlannedBackend, error) {
	planned, err := l.plan(GroupBackend, fmt.Sprintf("sandbox %q", entry.Name), entry.Impl, entry.Settings)
	if err != nil {
		return PlannedBackend{}, err
	}
	build := planned.make
	return PlannedBackend{
		Name: entry.Name,
		Impl: planned.impl,
		New: func() (func(sandbox.Confine) (sandbox.Runner, error), error) {
			built, err := build()
			if err != nil {
				return nil, err
			}
			factory, ok := built.(func(sandbox.Confine) (sandbox.Runner, error))
			if !ok {
				return nil, errdefs.Internalf(
					"deploy: sandbox impl %q built %T, want a per-session constructor", planned.impl, built)
			}
			return factory, nil
		},
	}, nil
}

func (l *Loader) planToolSource(entry entryDocument) (PlannedToolSource, error) {
	planned, err := l.plan(GroupToolSource, fmt.Sprintf("tool source %q", entry.Name), entry.Impl, entry.Settings)
	if err != nil {
		return PlannedToolSource{}, err
	}
	build := planned.make
	return PlannedToolSource{
		Name: entry.Name,
		Impl: planned.impl,
		New: func() (tool.Source, error) {
			built, err := build()
			if err != nil {
				return nil, err
			}
			source, ok := built.(tool.Source)
			if !ok {
				return nil, errdefs.Internalf(
					"deploy: tool source kind %q built %T, want tool.Source", planned.impl, built)
			}
			return source, nil
		},
	}, nil
}

func (l *Loader) planMiddleware(entry entryDocument) (PlannedMiddleware, error) {
	planned, err := l.plan(GroupMiddleware, fmt.Sprintf("middleware %q", entry.Name), entry.Impl, entry.Settings)
	if err != nil {
		return PlannedMiddleware{}, err
	}
	build := planned.make
	return PlannedMiddleware{
		Name:  entry.Name,
		Impl:  planned.impl,
		Trace: planned.trace,
		New: func() (func(tool.Catalog) (tool.Middleware, error), error) {
			built, err := build()
			if err != nil {
				return nil, err
			}
			factory, ok := built.(func(tool.Catalog) (tool.Middleware, error))
			if !ok {
				return nil, errdefs.Internalf(
					"deploy: middleware impl %q built %T, want a catalog-aware constructor", planned.impl, built)
			}
			return factory, nil
		},
	}, nil
}

func (l *Loader) planProgram(entry programDocument) (PlannedProgram, error) {
	planned, err := l.plan(GroupProgram, "program", entry.Impl, entry.Settings)
	if err != nil {
		return PlannedProgram{}, err
	}
	build := planned.make
	return PlannedProgram{
		Impl: planned.impl,
		New: func() (agent.Program, error) {
			built, err := build()
			if err != nil {
				return nil, err
			}
			program, ok := built.(agent.Program)
			if !ok {
				return nil, errdefs.Internalf(
					"deploy: program kind %q built %T, want agent.Program", planned.impl, built)
			}
			return program, nil
		},
	}, nil
}

// planned is the family-neutral result of one resolution.
type planned struct {
	impl  string
	trace telemetry.TraceLevel
	make  func() (any, error)
}

// plan is the one path every family takes: find the implementation, then decode the settings
// with it.
//
// label names the entry in an error message ("provider \"gpt\"", "program"), because the
// program entry has no name of its own — the agent that owns it does.
func (l *Loader) plan(group Group, label, impl string, raw json.RawMessage) (planned, error) {
	impl = strings.TrimSpace(impl)
	if impl == "" {
		return planned{}, errdefs.Validationf(
			"deploy: %s names no impl; a document must say what implements it", label)
	}
	factory, ok := l.impls.factory(group, impl)
	if !ok {
		return planned{}, errdefs.Validationf(
			"deploy: %s selects impl %q, which is not registered (%s)",
			label, impl, listOf(l.impls.Names(group)))
	}
	settings, err := factory.Plan(raw)
	if err != nil {
		return planned{}, err
	}
	return planned{
		impl:  impl,
		trace: factory.Trace,
		make:  func() (any, error) { return factory.Make(settings) },
	}, nil
}

// ready refuses a broken registration table once, rather than per document.
func (l *Loader) ready() error {
	if l == nil || l.impls == nil {
		return errdefs.Validationf("deploy: a loader requires a kind table")
	}
	if err := l.impls.Err(); err != nil {
		return err
	}
	return nil
}

// takeName enforces one namespace per section: a document that names two providers the same
// thing cannot be told apart by a grant that selects one of them.
func takeName(seen map[string]struct{}, group, name string) error {
	if name == "" {
		return errdefs.Validationf("deploy: a %s entry needs a name", group)
	}
	if _, exists := seen[group+"/"+name]; exists {
		return errdefs.Validationf("deploy: %s %q is declared twice", group, name)
	}
	seen[group+"/"+name] = struct{}{}
	return nil
}

// listOf renders a name list for an error message, saying "none" rather than nothing.
func listOf(names []string) string {
	if len(names) == 0 {
		return "none registered"
	}
	return strings.Join(names, ", ")
}
