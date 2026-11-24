// Resource is the process's resources: the model targets it serves, the backends it can run a
// session in, and the tool table every session dispatches through.
//
// It is the passive half of this package. Everything here is built once, from a
// [deploy.ProcessPlan], and closed once when the process exits; the active half — the agent
// table, the sessions, the runs — is [Agents], which holds a *Resource and reads it directly.
//
// Every build step can fail, and each failure has to undo the steps before it: a process that
// leaked three backends because the fourth failed is worse than one that refused to start. So
// [Open] records a closer per resource as it builds and runs them in reverse on any error,
// joining the cleanup failures rather than replacing the original one.
package host

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/skill"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/dispatch"
)

// Resources is a built process. Its fields are unexported because there is exactly one thing a
// caller may do with each of them, and handing out the fields would let a caller close one.
type Resource struct {
	providers []*inference.Provider
	models    *inference.Provider
	backends  map[string]BackendFactory
	order     []string
	// skills is the catalog discovered from the plan's roots, once for the whole process: the tools
	// search what the host reports, so there is one discovery and not one per session.
	skills   *skill.Index
	tools    *tool.Registry
	dispatch tool.Dispatcher
	// observability is how much of a turn this process records. It is one value for the whole
	// process because everything it controls — the tool chain, the model access, the backends —
	// is built once here, and a level two agents could disagree about would be a knob with no
	// effect.
	observability deploy.Observability
	closers       []closer
}

// BackendFactory builds one session's backend instance from that session's confine.
//
// The factory is process-wide; the instance is not. A backend enforces what the grant allows —
// the write set, the network posture, the environment — and the grant is per session, so the
// instance is built when the session's boundary is, and closed with it.
type BackendFactory = func(sandbox.Confine) (sandbox.Runner, error)

// closer is one resource to release, with the name an error message needs.
type closer struct {
	name  string
	close func() error
}

// Options configures [Open].
type Options struct {
	// Capabilities is what this process can hand a tool, declared by the layer that fills it per
	// session. Registration refuses a tool whose Needs the set cannot satisfy, so a tool that
	// would read an absent capability fails here rather than on its first call.
	//
	// Nil means "declare nothing": every tool that needs a capability is refused, which is the
	// honest default for a caller that did not say what it can provide.
	Capabilities *tool.CapSet
}

// Open builds the process described by plan.
//
// The build order is the dependency order — providers, then the routed provider over them, then
// backends, then the tool sources, then the chain over them — and Close runs it backwards. The
// routed provider is built even for a single target: one seam that always routes keeps callers
// from branching on "is there more than one".
func Open(ctx context.Context, plan deploy.ProcessPlan, opts ...Options) (*Resource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var o Options
	for _, opt := range opts {
		o = opt
	}

	r := &Resource{
		backends:      make(map[string]BackendFactory, len(plan.Backends)),
		observability: plan.Observability,
	}
	// unwind releases what was built so far, keeping the original error as the cause.
	unwind := func(err error) (*Resource, error) {
		return nil, errors.Join(err, r.Close())
	}

	// The skill catalog is discovered here, over a read-only tree of the roots the deployment named,
	// and the tree is released at once: what a session needs later is the index, not the handles.
	if skills, err := buildSkills(ctx, plan.SkillRoots); err != nil {
		return unwind(err)
	} else {
		r.skills = skills
	}

	for _, planned := range plan.Providers {
		provider, err := planned.New()
		if err != nil {
			return unwind(fmt.Errorf("resource: provider %q: %w", planned.Name, err))
		}
		r.providers = append(r.providers, provider)
	}
	routed, err := r.buildRouted(plan)
	if err != nil {
		return unwind(err)
	}
	r.models = routed

	for _, planned := range plan.Backends {
		factory, err := planned.New()
		if err != nil {
			return unwind(fmt.Errorf("resource: sandbox %q: %w", planned.Name, err))
		}
		if factory == nil {
			return unwind(fmt.Errorf("resource: sandbox %q built no constructor", planned.Name))
		}
		r.backends[planned.Name] = factory
		r.order = append(r.order, planned.Name)
	}

	sources, err := r.buildToolSources(plan)
	if err != nil {
		return unwind(err)
	}
	registry, err := tool.NewRegistry(sources, tool.WithProvisioned(o.Capabilities))
	if err != nil {
		return unwind(fmt.Errorf("resource: tool table: %w", err))
	}
	r.tools = registry
	r.addCloser("tool table", registry.Close)

	chain, err := r.buildChain(plan, registry)
	if err != nil {
		return unwind(err)
	}
	r.dispatch = dispatch.New(registry, chain...)
	return r, nil
}

// buildRouted wraps the built providers in one router.
//
// A target carries no model of its own: what a provider serves is its catalog's answer, and
// declaring the model here as well would be a second spelling of the same fact. The request names
// a model; the router keeps the targets whose catalog lists it.
func (r *Resource) buildRouted(plan deploy.ProcessPlan) (*inference.Provider, error) {
	targets := make([]route.Target, 0, len(r.providers))
	for i, planned := range plan.Providers {
		targets = append(targets, route.Target{Name: planned.Name, Provider: *r.providers[i]})
	}
	if len(targets) == 0 {
		return nil, nil
	}
	router, err := route.New(targets, route.Policy{Retry: plan.Route})
	if err != nil {
		return nil, fmt.Errorf("resource: route: %w", err)
	}
	provider := router.Provider()
	return &provider, nil
}

func (r *Resource) buildToolSources(plan deploy.ProcessPlan) ([]tool.Source, error) {
	sources := make([]tool.Source, 0, len(plan.Tools))
	for _, planned := range plan.Tools {
		source, err := planned.New()
		if err != nil {
			return nil, fmt.Errorf("resource: tool source %q: %w", planned.Name, err)
		}
		if source == nil {
			return nil, fmt.Errorf("resource: tool source %q built nothing", planned.Name)
		}
		sources = append(sources, source)
	}
	return sources, nil
}

// buildChain resolves the middleware the document declared.
//
// A link that returns nil from its constructor is disabled — that is how a deployment turns one
// off without a second flag — and the chain simply does not carry it.
func (r *Resource) buildChain(plan deploy.ProcessPlan, catalog tool.Catalog) ([]tool.Middleware, error) {
	chain := make([]tool.Middleware, 0, len(plan.Middleware))
	for _, planned := range plan.Middleware {
		build, err := planned.New()
		if err != nil {
			return nil, fmt.Errorf("resource: middleware %q: %w", planned.Name, err)
		}
		middleware, err := build(catalog)
		if err != nil {
			return nil, fmt.Errorf("resource: middleware %q: %w", planned.Name, err)
		}
		if middleware == nil {
			continue
		}
		chain = append(chain, middleware)
	}
	return chain, nil
}

func (r *Resource) addCloser(name string, close func() error) {
	r.closers = append(r.closers, closer{name: name, close: close})
}

// backendFactory returns the constructor registered under name, which is what a grant's sandbox
// field selects.
//
// It is unexported because [Agents] reads it directly: a session's backend instance is built from
// this factory and that session's confine, so the only consumer is in this package.
func (r *Resource) backendFactory(name string) (BackendFactory, bool) {
	factory, ok := r.backends[name]
	return factory, ok
}

// Capabilities reports what an agent document may ask for.
//
// It is the only exported read surface: the rest of a Resource is consumed by [Agents] in this
// package, and exporting it would let a caller close a piece of the process.
//
// The model list is read here, after the build, because it is the only place it can come from: a
// provider's settings are opaque to the document layer, and what a provider serves is its
// catalog's answer.
func (r *Resource) Capabilities() deploy.Capabilities {
	caps := deploy.Capabilities{Backends: append([]string(nil), r.order...)}
	seen := make(map[string]struct{}, len(caps.Backends))
	for _, provider := range r.providers {
		if provider.Catalog == nil {
			continue
		}
		for _, model := range provider.Catalog.Models() {
			ref := model.ID.Provider + "/" + model.ID.Name
			if _, duplicate := seen[ref]; duplicate {
				continue
			}
			seen[ref] = struct{}{}
			caps.Models = append(caps.Models, ref)
		}
	}
	sort.Strings(caps.Models)
	return caps
}

// Close releases every resource, in the reverse of the order they were built: the tool table
// first (it may still hold tools that reference a backend), then the backends, and last whatever
// came before them.
//
// It is safe to call more than once, and it joins the failures instead of stopping at the first:
// one backend refusing to close is not a reason to leak the other.
func (r *Resource) Close() error {
	errs := make([]error, 0, len(r.closers))
	for i := len(r.closers) - 1; i >= 0; i-- {
		c := r.closers[i]
		if err := c.close(); err != nil {
			errs = append(errs, fmt.Errorf("resource: closing %s: %w", c.name, err))
		}
	}
	r.closers = nil
	r.tools = nil
	r.dispatch = nil
	r.backends = nil
	r.order = nil
	r.providers = nil
	r.models = nil
	return errors.Join(errs...)
}

// buildSkills discovers the deployment's skill roots and indexes them.
//
// It reads through a tree of those roots and closes it immediately: the index is the result, and a
// process that kept the handles would keep file descriptors for a catalog it never reads again. A
// session reads a skill body through its own boundary, so a grant that does not include the skill
// roots still cannot read them — the index says what exists, the tree decides what is reachable.
func buildSkills(ctx context.Context, roots []string) (*skill.Index, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	tree, err := fsx.Open(roots)
	if err != nil {
		return nil, fmt.Errorf("host: skill roots: %w", err)
	}
	defer func() { _ = tree.Close() }()
	discovered := skill.Discover(ctx, tree, roots)
	if len(discovered.Skills) == 0 && len(discovered.Diagnostics) > 0 {
		return nil, fmt.Errorf("host: skill discovery: %s", discovered.Diagnostics[0].Message)
	}
	index, err := skill.NewIndex(discovered.Skills)
	if err != nil {
		return nil, fmt.Errorf("host: skill index: %w", err)
	}
	return index, nil
}
